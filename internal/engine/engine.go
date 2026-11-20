// Package engine 把 observe → decide → budget → 执行粘起来（§3.8/§3.9）。
// 动作互相隔离：一个失败只记日志并回填状态（退避/事故证据），不影响同轮
// 其他动作；执行顺序 RemoveReg/Drain/ForceDelete → Create → SSHAct → Incident
// （decide 已按此序输出，engine 按序执行）。
package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/observe"
)

// PublicTag 是编排器所辖 droplet 的公共 DO tag（§3.5）；编排器自身用
// ciorch-host（不匹配公共 tag，不计入全局配额）。
const PublicTag = "ciorch"

// GHFace 是 engine 需要的 GitHub 侧写能力（githubapp.Client 实现，测试可 fake）。
type GHFace interface {
	MintToken(ctx context.Context, repo string) (string, error)
	RemoveRunner(ctx context.Context, repo string, runnerID int64) error
	ListRunners(ctx context.Context, repo string) ([]observe.Runner, error)
}

// VPSFace 是 engine 需要的 DO 侧写能力（vpsctlapi.Client 实现）。
type VPSFace interface {
	Create(ctx context.Context, o CreateOpts) (CreatedDroplet, error)
	Delete(ctx context.Context, account, id string) error
	ListByTag(ctx context.Context, tag, account string) ([]Droplet, error)
}

// CreateOpts 镜像 vpsctlapi.CreateOpts（解耦 engine 与子进程包，便于 fake）。
type CreateOpts struct {
	Account, Region, Size, Image, NamePrefix string
	StartIndex                               int
	Tags                                     []string
	UserDataFile                             string
}

// CreatedDroplet 是建机受理结果（IP 可空）。
type CreatedDroplet struct {
	Account, ID, Name, IPv4Public string
}

// Droplet 镜像 vpsctlapi.Droplet 的 engine 视图。
type Droplet struct {
	Account      string
	ID, Name     string
	PriceMonthly float64
	Tags         []string
}

// SSHFace 经系统 ssh 子进程调机器上的 cirunner act（§3.8）。
type SSHFace interface {
	Act(ctx context.Context, ip, action string) ([]byte, error)
}

// CloudRenderer 渲染 cloud-init（真实现来自 cirunner 仓 pkg/cloudinit，阶段 3
// 机器侧重写后接入；此前 dry-run/测试用 fake）。
type CloudRenderer interface {
	Render(o RenderParams) ([]byte, error)
}

// RenderParams 是渲染入参的 engine 视图。
type RenderParams struct {
	RepoURL, ServiceRepo, RunnerName string
	Labels                           []string
	Token                            string
	PublicKeys                       []string
}

// Engine 是执行面。
type Engine struct {
	Cfg      *config.Config
	GH       GHFace
	VPS      VPSFace
	SSH      SSHFace
	Renderer CloudRenderer
	Log      *log.Logger

	// Collector 由 daemon 注入（plan/status 直接观察也走它）；接口化便于测试。
	Collector Observer

	// States 是各仓决策记忆（daemon 持有并持久化）。
	States map[string]decide.RepoState
	// OnIncident 是事故回调（阶段 5 接 incident 模块；nil = 仅记日志）。
	OnIncident func(ctx context.Context, repo string, a decide.Action)

	stateDir string
}

// New 构造。
func New(cfg *config.Config, gh GHFace, vps VPSFace, ssh SSHFace, r CloudRenderer, lg *log.Logger) *Engine {
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	return &Engine{
		Cfg: cfg, GH: gh, VPS: vps, SSH: ssh, Renderer: r, Log: lg,
		States:   map[string]decide.RepoState{},
		stateDir: cfg.StateDir,
	}
}

// Observer 是一轮观察能力（observe.Collector 实现，测试可 fake）。
type Observer interface {
	Collect(ctx context.Context, r config.RepoEntry, p config.Profile) (observe.Obs, error)
}

// SetCollector 注入观察器。
func (e *Engine) SetCollector(c Observer) { e.Collector = c }

// Reconcile 对一个仓走一轮：observe → decide → budget 分配 → 执行。
// dryRun = 只观察决策写日志不执行。createBudget 是本轮该仓允许的建机配额
// （由上层 budget.Allocate 统一分配后传入；nil = 不限）。
func (e *Engine) Reconcile(ctx context.Context, repo config.RepoEntry, dryRun bool, createBudget *int) ([]decide.Action, error) {
	if e.Collector == nil {
		return nil, fmt.Errorf("collector 未注入")
	}
	p := e.Cfg.ProfileOf(repo)
	obs, err := e.Collector.Collect(ctx, repo, p)
	if err != nil {
		e.Log.Printf("[%s] 观察不完整: %v", repo.Repo, err)
	}
	params := decide.Params{
		MaxRunners:        repo.MaxRunners,
		RunnersPerDroplet: p.RunnersPerDroplet,
		OwnedRunnerRe:     regexp.MustCompile(repo.NamespacePattern(p)),
	}
	acts, st := decide.Decide(params, e.States[repo.Repo], obs)
	e.States[repo.Repo] = st
	if dryRun {
		for _, a := range acts {
			e.Log.Printf("[%s] dry-run %s: %s", repo.Repo, a.Kind, a.Reason)
		}
		return acts, nil
	}
	e.execute(ctx, repo, obs, acts, createBudget)
	return acts, nil
}

// execute 按序执行动作；动作隔离（§3.8）：失败记日志/回填，不中断。
func (e *Engine) execute(ctx context.Context, repo config.RepoEntry, obs observe.Obs, acts []decide.Action, createBudget *int) {
	for _, a := range acts {
		switch a.Kind {
		case decide.ActCreate:
			if createBudget != nil && *createBudget <= 0 {
				e.Log.Printf("[%s] 跳过建机：本轮配额已用尽（场景 F）", repo.Repo)
				continue
			}
			if err := e.create(ctx, repo, obs); err != nil {
				e.Log.Printf("[%s] 建机失败: %v", repo.Repo, err)
				st := e.States[repo.Repo]
				if inc, fire := decide.RecordCreateResult(decide.Params{}, &st, time.Now(), err); fire {
					e.fireIncident(ctx, repo, inc)
				}
				e.States[repo.Repo] = st
			} else if createBudget != nil {
				*createBudget--
			}
		case decide.ActDrain:
			// 先注销注册，成功后才删机（防 #7 竞态；注销失败本轮放弃删机）
			if err := e.GH.RemoveRunner(ctx, repo.Repo, a.RunnerID); err != nil {
				e.Log.Printf("[%s] Drain 注销失败，保留机器下轮重判: %v", repo.Repo, err)
				continue
			}
			if err := e.VPS.Delete(ctx, e.Cfg.ProfileOf(repo).Account, a.DropletID); err != nil {
				e.Log.Printf("[%s] Drain 删机失败: %v", repo.Repo, err)
				continue
			}
			e.Log.Printf("[%s] 已回收 %s（%s）", repo.Repo, a.DropletName, a.Reason)
		case decide.ActForceDelete:
			// 强删：先删 droplet 再清注册（job 已判死/注册从未成功）
			if err := e.VPS.Delete(ctx, e.Cfg.ProfileOf(repo).Account, a.DropletID); err != nil {
				e.Log.Printf("[%s] 强删失败: %v", repo.Repo, err)
				continue
			}
			if a.RunnerID != 0 {
				_ = e.GH.RemoveRunner(ctx, repo.Repo, a.RunnerID)
			} else if a.RunnerName != "" {
				e.removeRunnerByName(ctx, repo, a.RunnerName)
			}
			e.Log.Printf("[%s] 已强删 %s（%s）", repo.Repo, a.DropletName, a.Reason)
		case decide.ActRemoveReg:
			if err := e.GH.RemoveRunner(ctx, repo.Repo, a.RunnerID); err != nil {
				e.Log.Printf("[%s] 清注册失败: %v", repo.Repo, err)
			}
		case decide.ActSSHAct:
			ip := dropletIP(obs, a.DropletName)
			if ip == "" {
				e.Log.Printf("[%s] SSHAct 跳过：%s 无 IP（观察里查不到）", repo.Repo, a.DropletName)
				continue
			}
			if _, err := e.SSH.Act(ctx, ip, a.SSHAct); err != nil {
				e.Log.Printf("[%s] SSHAct %s 失败: %v", repo.Repo, a.SSHAct, err)
			}
		case decide.ActIncident:
			e.fireIncident(ctx, repo, a)
		}
	}
}

// removeRunnerByName 按名清理注册（ForceDelete 时 id 缺失的兜底；顺带清
// rpd=2 的 -a/-b 实例）。
func (e *Engine) removeRunnerByName(ctx context.Context, repo config.RepoEntry, name string) {
	runners, err := e.GH.ListRunners(ctx, repo.Repo)
	if err != nil {
		e.Log.Printf("[%s] 按名清注册失败（列 runner）: %v", repo.Repo, err)
		return
	}
	for _, r := range runners {
		if r.Name == name || r.Name == name+"-a" || r.Name == name+"-b" {
			if err := e.GH.RemoveRunner(ctx, repo.Repo, r.ID); err != nil {
				e.Log.Printf("[%s] 清注册 %s 失败: %v", repo.Repo, r.Name, err)
			}
		}
	}
}

func (e *Engine) fireIncident(ctx context.Context, repo config.RepoEntry, a decide.Action) {
	e.Log.Printf("[%s] 事故 %s/%s: %s", repo.Repo, a.IncidentType, a.IncidentKey, a.Reason)
	if e.OnIncident != nil {
		e.OnIncident(ctx, repo.Repo, a)
	}
}

// create：铸 token → 渲染 cloud-init → 写临时文件（0600；成功即删，失败留存
// 供诊断）→ vpsctl create（不带 -wait）。
func (e *Engine) create(ctx context.Context, repo config.RepoEntry, obs observe.Obs) error {
	p := e.Cfg.ProfileOf(repo)
	name := decide.NextName(repo.NamePrefix(), p.Account, p.Region, convertDroplets(obs.Droplets))
	ctxTO, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	token, err := e.GH.MintToken(ctxTO, repo.Repo)
	if err != nil {
		return fmt.Errorf("铸 registration token: %w", err)
	}
	ci, err := e.Renderer.Render(RenderParams{
		RepoURL:     "https://github.com/" + repo.Repo,
		ServiceRepo: serviceRepo(repo.Repo),
		RunnerName:  name,
		Labels:      p.Labels,
		Token:       token,
		PublicKeys:  e.publicKeys(),
	})
	if err != nil {
		return fmt.Errorf("渲染 cloud-init: %w", err)
	}
	dir := filepath.Join(e.stateDirOrDefault(), "cloudinit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// 文件含 1h 时效 token：建机成功即删；失败留存供排查（目录 0700）
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, ci, 0o600); err != nil {
		return err
	}
	d, err := e.VPS.Create(ctxTO, CreateOpts{
		Account: p.Account, Region: p.Region, Size: p.Size, Image: p.Image,
		NamePrefix:   repo.NamePrefix(),
		StartIndex:   nameIndex(repo, p, name),
		Tags:         []string{PublicTag, repo.Tag()},
		UserDataFile: path,
	})
	if err != nil {
		return err // user-data 留存
	}
	_ = os.Remove(path)
	e.Log.Printf("[%s] 已建机 %s (id=%s, ip=%q)", repo.Repo, d.Name, d.ID, d.IPv4Public)
	return nil
}

func (e *Engine) publicKeys() []string {
	var keys []string
	for _, f := range e.Cfg.SSHPublicKeyFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			e.Log.Printf("读公钥 %s 失败: %v", f, err)
			continue
		}
		keys = append(keys, string(raw))
	}
	return keys
}

func (e *Engine) stateDirOrDefault() string {
	if e.stateDir != "" {
		return e.stateDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ciorch")
}

// ScanOrphans 场景 J：全局列公共 tag，droplet 不带任何已配置仓 tag → 只告警
// **绝不删**（等人确认）。
func (e *Engine) ScanOrphans(ctx context.Context) ([]Droplet, error) {
	repoTags := map[string]bool{}
	accounts := map[string]bool{}
	for _, r := range e.Cfg.Repos {
		repoTags[r.Tag()] = true
		accounts[e.Cfg.ProfileOf(r).Account] = true
	}
	seen := map[string]bool{}
	var orphans []Droplet
	for acc := range accounts {
		ds, err := e.VPS.ListByTag(ctx, PublicTag, acc)
		if err != nil {
			return nil, fmt.Errorf("账号 %s 列机: %w", acc, err)
		}
		for _, d := range ds {
			if seen[d.ID] {
				continue
			}
			seen[d.ID] = true
			if !hasAnyTag(d.Tags, repoTags) {
				orphans = append(orphans, d)
			}
		}
	}
	return orphans, nil
}

func hasAnyTag(tags []string, want map[string]bool) bool {
	for _, t := range tags {
		if want[t] {
			return true
		}
	}
	return false
}

func dropletIP(obs observe.Obs, name string) string {
	for _, d := range obs.Droplets {
		if d.Name == name {
			if d.PrivateIP != "" {
				return d.PrivateIP
			}
			return d.PublicIP
		}
	}
	return ""
}

func convertDroplets(ds []observe.Droplet) []observe.Droplet { return ds }

func nameIndex(repo config.RepoEntry, p config.Profile, name string) int {
	prefix := fmt.Sprintf("%s-%s-%s-", repo.NamePrefix(), p.Account, p.Region)
	var n int
	fmt.Sscanf(name[len(prefix):], "%d", &n)
	return n
}

// serviceRepo 把 owner/name 转成 systemd unit 用的连字符形式（坑 4：点号会
// Unit not found）。
func serviceRepo(repo string) string {
	out := make([]byte, 0, len(repo))
	for i := 0; i < len(repo); i++ {
		c := repo[i]
		if c == '/' || c == '.' {
			c = '-'
		}
		out = append(out, c)
	}
	return string(out)
}
