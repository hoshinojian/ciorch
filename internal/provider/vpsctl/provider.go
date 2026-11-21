// Package vpsctl 是 vpsctl（DigitalOcean）的 machine.Provider 实现
// （docs/PROVIDER-ABSTRACTION.md §3.5）。子进程封装在 client.go（自
// internal/vpsctlapi 并入，旧包已删除）。业务包只允许依赖 internal/machine，
// 不得 import 本包（CI import 检查强制）。
package vpsctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/hoshinojian/ciorch/internal/machine"
)

// regionRe：DO region slug 形如 sgp1/nyc3/blr1（无连字符）。
var regionRe = regexp.MustCompile(`^[a-z]+[0-9]+$`)

// hoursPerMonth 是 DO 月封顶小时数口径（REDESIGN §3.7）：
// Machine.HourlyCost = price_monthly / 672。
const hoursPerMonth = 672.0

// Options 是 vpsctl 池的建机参数（池配置的 "vpsctl" 子对象，§3.3）。
type Options struct {
	Bin          string `json:"bin"`
	AccountsFile string `json:"accounts_file"` // 可选 -accounts（CI 专用账号文件，REDESIGN §2.3）
	Account      string `json:"account"`       // vpsctl -only 钉账号（防全账号误建，2026-11-13 教训）
	Region       string `json:"region"`
	Size         string `json:"size"`
	Image        string `json:"image"`
	Snapshot     string `json:"snapshot"` // 阶段 7 快照；非空时建机用它替代 Image
	// PreferPrivateIP 为 nil 或 true 时 Machine.Addr 优先私网（同区同 VPC
	// 内网直连）；显式 false 用公网。
	PreferPrivateIP *bool   `json:"prefer_private_ip"`
	HourlyCostHint  float64 `json:"hourly_cost_hint"` // 空池建新机的保守估价（USD/h，必填>0）
}

func (o Options) preferPrivateIP() bool {
	return o.PreferPrivateIP == nil || *o.PreferPrivateIP
}

// buildImage 返回建机实际用的镜像（快照优先，§3.3 snapshot）。
func (o Options) buildImage() string {
	if o.Snapshot != "" {
		return o.Snapshot
	}
	return o.Image
}

// Provider 实现一个 vpsctl 池（account+region+size+image 组合）。
// 并发安全（machine.Provider 契约）。
type Provider struct {
	pool    string
	opt     Options
	client  *Client
	dataDir string // cloud-init 落盘目录（0700；建机成功即删，失败留存供诊断）
	nam     namer

	failList atomic.Bool // 测试钩子：契约套件的失败注入（生产路径不设置）
}

// Factory 解析池配置的 "vpsctl" 子对象并构造 Provider（config.Load 启动期
// 也会调用一次做校验）。注册名 "vpsctl"。
func Factory(pool string, raw json.RawMessage, env machine.Env) (machine.Provider, error) {
	var o Options
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return nil, fmt.Errorf("pools.%s.vpsctl: %w", pool, err)
	}
	for _, c := range []struct {
		key, val string
	}{
		{"bin", o.Bin}, {"account", o.Account}, {"region", o.Region},
		{"size", o.Size}, {"image", o.Image},
	} {
		if c.val == "" {
			return nil, fmt.Errorf("pools.%s.vpsctl.%s：必填", pool, c.key)
		}
	}
	if !regionRe.MatchString(o.Region) {
		return nil, fmt.Errorf("pools.%s.vpsctl.region：%q 不是 DO region slug 形式", pool, o.Region)
	}
	if o.HourlyCostHint <= 0 {
		return nil, fmt.Errorf("pools.%s.vpsctl.hourly_cost_hint：必须 >0（空池建机的预算投影不能当 0，否则 DO 池会绕过预算）", pool)
	}
	if env.StateDir == "" {
		return nil, fmt.Errorf("pools.%s：缺少 state_dir（cloud-init 落盘目录的根）", pool)
	}
	return NewProvider(pool, o, filepath.Join(env.StateDir, "cloudinit")), nil
}

func init() { machine.Register("vpsctl", Factory) }

// NewProvider 构造。userDataDir 是 cloud-init 文件目录（内容含 1h 时效
// registration token：0600 落盘、建机成功即删）。
func NewProvider(pool string, opt Options, userDataDir string) *Provider {
	return &Provider{
		pool: pool, opt: opt, dataDir: userDataDir,
		client: NewClient(opt.Bin, opt.AccountsFile),
		nam:    namer{account: opt.Account, region: opt.Region},
	}
}

// Kind 实现 machine.Provider。
func (p *Provider) Kind() string { return "vpsctl" }

// nameRe 返回本池的机器名解析式：{prefix}-{account}-{region}-NN。
func (p *Provider) nameRe() *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(`^(.*)-%s-%s-(\d+)$`,
		regexp.QuoteMeta(p.opt.Account), regexp.QuoteMeta(p.opt.Region)))
}

// splitName 把 ciorch 定好的机器名解析回 vpsctl 的 -name-prefix 与
// -start-index（机器名 = runner 名，vpsctl 自己拼 {prefix}-{account}-{region}-NN）。
func (p *Provider) splitName(name string) (prefix string, startIndex int, err error) {
	m := p.nameRe().FindStringSubmatch(name)
	if m == nil {
		return "", 0, fmt.Errorf("机器名 %q 不符合本池命名格式 {prefix}-%s-%s-NN", name, p.opt.Account, p.opt.Region)
	}
	n, _ := strconv.Atoi(m[2])
	return m[1], n, nil
}

// Create 建一台机：cloud-init 落盘（0600）→ vpsctl create（不带 -wait，受理
// 即返回、IP 可为空）。名字必须符合本池命名格式（拆回 prefix+start-index），
// 否则报错且不调 vpsctl。建机失败 user-data 留存供诊断，成功即删。
func (p *Provider) Create(ctx context.Context, req machine.CreateRequest) (machine.Machine, error) {
	if len(req.UserData) == 0 {
		return machine.Machine{}, fmt.Errorf("UserData 必填（渲染好的 cloud-init）")
	}
	prefix, start, err := p.splitName(req.Name)
	if err != nil {
		return machine.Machine{}, err
	}
	if err := os.MkdirAll(p.dataDir, 0o700); err != nil {
		return machine.Machine{}, fmt.Errorf("建 cloud-init 目录: %w", err)
	}
	path := filepath.Join(p.dataDir, req.Name+".yaml")
	if err := os.WriteFile(path, req.UserData, 0o600); err != nil {
		return machine.Machine{}, fmt.Errorf("写 cloud-init: %w", err)
	}
	d, err := p.client.Create(ctx, CreateOpts{
		Account: p.opt.Account, Region: p.opt.Region, Size: p.opt.Size, Image: p.opt.buildImage(),
		NamePrefix: prefix, StartIndex: start,
		Tags: req.Tags, UserDataFile: path,
	})
	if err != nil {
		return machine.Machine{}, err // user-data 留存
	}
	_ = os.Remove(path)
	return p.toMachine(d), nil
}

// Delete 删机（vpsctl -ids account/id -confirm 1）。已不存在的机器 vpsctl
// 返回 ok:true（DO 404 在 vpsctl 的 DO 客户端折算为成功，
// vpsctl/internal/provider/digitalocean/client.go Delete 注释），故幂等返回 nil。
func (p *Provider) Delete(ctx context.Context, id string) error {
	return p.client.Delete(ctx, p.opt.Account, id)
}

// List 按标记精确过滤列机（-only 钉本池账号）。errors[] 非空已被
// vpsctlapi 折算为 error——绝不返回不完整列表（观察冻结依赖这一点）。
func (p *Provider) List(ctx context.Context, tag string) ([]machine.Machine, error) {
	if p.failList.CompareAndSwap(true, false) {
		return nil, fmt.Errorf("vpsctl list: 注入的失败（契约套件）")
	}
	ds, err := p.client.ListByTag(ctx, tag, p.opt.Account)
	if err != nil {
		return nil, err
	}
	out := make([]machine.Machine, 0, len(ds))
	for _, d := range ds {
		out = append(out, p.toMachine(d))
	}
	return out, nil
}

// Namer 返回本池命名规则。
func (p *Provider) Namer() machine.Namer { return p.nam }

// FailNextList 注入下一次 List 失败（测试钩子，供契约套件用例；
// 生产路径不调用）。
func (p *Provider) FailNextList() { p.failList.Store(true) }

// HourlyCostHint 实现 machine.CostHinter（空池估价透出给预算层）。
func (p *Provider) HourlyCostHint() float64 { return p.opt.HourlyCostHint }

// toMachine 把 vpsctl 视图折成提供商无关视图。Addr：私网优先（可配），
// 缺私网用公网，都缺为空（engine 侧跳过 ssh）。
func (p *Provider) toMachine(d Droplet) machine.Machine {
	addr := d.IPv4Public
	if p.opt.preferPrivateIP() && d.IPv4Private != "" {
		addr = d.IPv4Private
	}
	return machine.Machine{
		Pool: p.pool, ID: d.ID, Name: d.Name, CreatedAt: d.CreatedAt,
		Addr: addr, HourlyCost: d.PriceMonthly / hoursPerMonth, Tags: d.Tags,
	}
}

// namer 是 vpsctl 池的命名规则：{prefix}-{account}-{region}-NN。
// 与旧 decide.NextName / config.NamespacePattern 逐字节一致（有一致性测试）。
type namer struct {
	account, region string
}

// Next 取下一个名字：同名空间最大序号 +1（跳号——被删机器的序号不回收）。
func (n namer) Next(prefix string, existing []string) string {
	pre := fmt.Sprintf("%s-%s-%s-", prefix, n.account, n.region)
	max := 0
	for _, name := range existing {
		suffix, ok := strings.CutPrefix(name, pre)
		if !ok {
			continue // 外部前缀/不同区域，不占本区序号
		}
		var num int
		if _, err := fmt.Sscanf(suffix, "%d", &num); err == nil && num > max {
			max = num
		}
	}
	return fmt.Sprintf("%s%02d", pre, max+1)
}

// Pattern 返回本池"属于这个前缀"的名字正则（命名空间外绝不触碰）。
func (n namer) Pattern(prefix string) string {
	return fmt.Sprintf(`^%s-%s-%s-\d{2}$`,
		regexp.QuoteMeta(prefix), regexp.QuoteMeta(n.account), regexp.QuoteMeta(n.region))
}

var _ machine.Provider = (*Provider)(nil)
