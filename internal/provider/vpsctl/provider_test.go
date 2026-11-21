package vpsctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/machine/machinetest"
)

var t0 = time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)

// ---------- fakeBin：模拟 vpsctl CLI（JSON 契约与 REDESIGN §4 对齐） ----------

// fakeBin 是注入的假子进程：按 vpsctl 的子命令/flag 契约响应，内存里维护
// 一台"DO"。不碰真实 DO。
type fakeBin struct {
	mu              sync.Mutex
	account, region string

	droplets    []Droplet
	nextID      int
	createCalls [][]string // 每次 create 的完整 argv
	userData    map[string][]byte
	createErr   bool // create 输出 errors[] 非空（部分失败）
	listErr     bool // list 输出 errors[] 非空（部分账号失败）
}

func newFakeBin(account, region string) *fakeBin {
	return &fakeBin{account: account, region: region, userData: map[string][]byte{}}
}

func (f *fakeBin) run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := args[0]
	flags := map[string]string{}
	for i := 1; i+1 < len(args); i += 2 {
		flags[args[i]] = args[i+1]
	}
	switch sub {
	case "create":
		f.createCalls = append(f.createCalls, append([]string(nil), args...))
		if f.createErr {
			return []byte(`{"batch":"b1","created":[],"errors":[{"account":"` + f.account + `","name":"","error":"DO 503"}]}`), nil
		}
		raw, _ := os.ReadFile(flags["-user-data"])
		f.userData[flags["-user-data"]] = raw
		start := 1
		if s := flags["-start-index"]; s != "" {
			fmt.Sscanf(s, "%d", &start)
		}
		f.nextID++
		name := fmt.Sprintf("%s-%s-%s-%02d", flags["-name-prefix"], f.account, f.region, start)
		d := Droplet{
			Account: f.account, ID: fmt.Sprintf("%d", 1000+f.nextID), Name: name,
			Status: "new", Region: f.region, Size: flags["-size"],
			IPv4Private: fmt.Sprintf("10.0.0.%d", f.nextID), PriceMonthly: 141,
			Tags: strings.Split(flags["-tags"], ","), CreatedAt: t0,
		}
		f.droplets = append(f.droplets, d)
		out, _ := json.Marshal(map[string]any{"batch": "b1", "created": []Droplet{d}})
		return out, nil
	case "list":
		if f.listErr {
			return []byte(`{"droplets":[],"errors":[{"account":"` + f.account + `","error":"token 失效"}]}`), nil
		}
		var kept []Droplet
		for _, d := range f.droplets {
			for _, tg := range d.Tags {
				if tg == flags["-tag"] {
					kept = append(kept, d)
					break
				}
			}
		}
		out, _ := json.Marshal(map[string]any{"droplets": kept})
		return out, nil
	case "delete":
		parts := strings.SplitN(flags["-ids"], "/", 2)
		var kept []Droplet
		for _, d := range f.droplets {
			if len(parts) == 2 && parts[1] == d.ID {
				continue
			}
			kept = append(kept, d)
		}
		f.droplets = kept
		out, _ := json.Marshal([]map[string]any{{"account": parts[0], "id": parts[1], "ok": true}})
		return out, nil
	}
	return nil, fmt.Errorf("fakeBin: 未知子命令 %q", sub)
}

// ---------- 构造 ----------

func newTestProvider(t *testing.T) (*Provider, *fakeBin, string) {
	t.Helper()
	bin := newFakeBin("team2-ci", "sgp1")
	dir := t.TempDir()
	p := NewProvider("test-pool", Options{
		Bin: "/usr/bin/fake-vpsctl", AccountsFile: "/tmp/vpsctl-accounts.json",
		Account: "team2-ci", Region: "sgp1", Size: "c2-4vcpu-8gb", Image: "ubuntu-24-04-x64",
		PreferPrivateIP: &[]bool{true}[0],
	}, dir)
	p.client.Run = bin.run
	return p, bin, dir
}

// ---------- 契约套件（§6：所有提供商必须通过） ----------

func TestProviderPassesContract(t *testing.T) {
	machinetest.Run(t, func(t *testing.T) machine.Provider {
		p, _, _ := newTestProvider(t)
		return p
	})
}

// ---------- vpsctl 实现专项（§6 vpsctl 条目） ----------

// Namer 与旧 decide.NextName / config.NamespacePattern 逐字节一致
// （跳号、外部前缀、不同区域、rpd=2 后缀、多位序号）。
// 字面期望值在 PR ③ 删除旧实现后仍锁行为；交叉对照在旧实现删除前先跑。
func TestNamerByteIdenticalWithLegacy(t *testing.T) {
	n := namer{account: "team2-ci", region: "sgp1"}
	cases := []struct {
		name     string
		existing []string
		want     string
	}{
		{"空池", nil, "ci-nms-team2-ci-sgp1-01"},
		{"跳号（被删序号不回收）", []string{"ci-nms-team2-ci-sgp1-01", "ci-nms-team2-ci-sgp1-03"}, "ci-nms-team2-ci-sgp1-04"},
		{"外部前缀不占号", []string{"ci-web-team2-ci-sgp1-07", "ci-nms-team2-ci-sgp1-01"}, "ci-nms-team2-ci-sgp1-02"},
		{"不同区域不占号", []string{"ci-nms-team2-ci-blr1-09", "ci-nms-team2-ci-sgp1-01"}, "ci-nms-team2-ci-sgp1-02"},
		{"rpd=2 的 runner 名在册（-a/-b 是 runner 名，不是机器名）", []string{"ci-nms-team2-ci-sgp1-01-a", "ci-nms-team2-ci-sgp1-01"}, "ci-nms-team2-ci-sgp1-02"},
		{"多位序号", []string{"ci-nms-team2-ci-sgp1-10"}, "ci-nms-team2-ci-sgp1-11"},
	}
	for _, c := range cases {
		got := n.Next("ci-nms", c.existing)
		if got != c.want {
			t.Errorf("%s: Next=%q want %q", c.name, got, c.want)
		}
		// 交叉对照旧 decide.NextName 的活体比对已在 PR ②（#18，两实现并存期）
		// 完成并留档；decide.NextName 随 PR ③ 删除，此处由字面期望值锁行为。
	}
	// 与旧 config.NamespacePattern 的活体比对已在 PR ②（#18）完成并留档；
	// NamespacePattern 随 PR ④ 池化配置删除，此处由字面值锁行为。
	if want := `^ci-nms-team2-ci-sgp1-\d{2}$`; n.Pattern("ci-nms") != want {
		t.Errorf("Pattern 字面值：%q want %q", n.Pattern("ci-nms"), want)
	}
}

// Create 传给 vpsctl 的 argv 与旧实现完全一致（不带 -wait；-only/-start-index/
// -tags/-user-data 原样；cloud-init 文件按机器名落盘）。
func TestCreateArgvMatchesLegacy(t *testing.T) {
	p, bin, dir := newTestProvider(t)
	name := p.Namer().Next("ci-nms", nil)
	if name != "ci-nms-team2-ci-sgp1-01" {
		t.Fatalf("Next 前置：got %q", name)
	}
	if _, err := p.Create(context.Background(), machine.CreateRequest{
		Name: name, Tags: []string{"ciorch", "ciorch-r-nms-abcdef12"},
		UserData: []byte("#cloud-config\n"),
	}); err != nil {
		t.Fatal(err)
	}
	got := bin.createCalls[0]
	want := []string{
		"create", "-accounts", "/tmp/vpsctl-accounts.json",
		"-only", "team2-ci", "-region", "sgp1", "-size", "c2-4vcpu-8gb",
		"-image", "ubuntu-24-04-x64", "-name-prefix", "ci-nms",
		"-count", "1", "-tags", "ciorch,ciorch-r-nms-abcdef12",
		"-user-data", filepath.Join(dir, name+".yaml"),
		"-start-index", "1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv 与旧实现不一致：\n got %v\nwant %v", got, want)
	}
	for _, a := range got {
		if a == "-wait" {
			t.Fatal("create 不得带 -wait（异步，不等 IP）")
		}
	}
}

// HourlyCost = price_monthly / 672；Addr 私网优先、缺私网用公网。
func TestListMapsCostAndAddr(t *testing.T) {
	p, bin, _ := newTestProvider(t)
	bin.mu.Lock()
	bin.droplets = []Droplet{
		{Account: "team2-ci", ID: "1", Name: "ci-nms-team2-ci-sgp1-01", Tags: []string{"ciorch"}, PriceMonthly: 141, IPv4Private: "10.0.0.5", IPv4Public: "1.2.3.4", CreatedAt: t0},
		{Account: "team2-ci", ID: "2", Name: "ci-nms-team2-ci-sgp1-02", Tags: []string{"ciorch"}, PriceMonthly: 141, IPv4Public: "5.6.7.8", CreatedAt: t0},
		{Account: "team2-ci", ID: "3", Name: "ci-nms-team2-ci-sgp1-03", Tags: []string{"ciorch"}, PriceMonthly: 141, CreatedAt: t0},
	}
	bin.mu.Unlock()
	ms, err := p.List(context.Background(), "ciorch")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("应列出 3 台：%+v", ms)
	}
	for _, m := range ms {
		if m.HourlyCost != 141.0/672.0 {
			t.Errorf("%s: HourlyCost=%v want %v", m.Name, m.HourlyCost, 141.0/672.0)
		}
		if m.Pool != "test-pool" {
			t.Errorf("%s: Pool=%q want test-pool", m.Name, m.Pool)
		}
	}
	if ms[0].Addr != "10.0.0.5" || ms[1].Addr != "5.6.7.8" || ms[2].Addr != "" {
		t.Fatalf("Addr 私网优先/公网回退/都缺为空：%v %v %q", ms[0].Addr, ms[1].Addr, ms[2].Addr)
	}
}

// 机器名解析失败必须报错且不调 vpsctl。
func TestCreateRejectsForeignName(t *testing.T) {
	p, bin, _ := newTestProvider(t)
	_, err := p.Create(context.Background(), machine.CreateRequest{
		Name: "ci-nms-team2-ci-sgp1-01-a", Tags: []string{"ciorch"}, UserData: []byte("#cloud-config\n"),
	})
	if err == nil || !strings.Contains(err.Error(), "不符合本池命名格式") {
		t.Fatalf("rpd=2 的 runner 名不是机器名，应拒绝：err=%v", err)
	}
	if _, err := p.Create(context.Background(), machine.CreateRequest{
		Name: "someone-elses-machine", Tags: []string{"ciorch"}, UserData: []byte("#cloud-config\n"),
	}); err == nil {
		t.Fatal("外部命名格式的名字应拒绝")
	}
	bin.mu.Lock()
	n := len(bin.createCalls)
	bin.mu.Unlock()
	if n != 0 {
		t.Fatalf("解析失败不得调 vpsctl：calls=%d", n)
	}
}

// cloud-init 文件生命周期：建机失败留存（诊断）、成功即删（含 1h token）。
func TestUserDataLifecycle(t *testing.T) {
	p, bin, dir := newTestProvider(t)
	name := p.Namer().Next("ci-nms", nil)
	path := filepath.Join(dir, name+".yaml")

	// 失败：留存
	bin.mu.Lock()
	bin.createErr = true
	bin.mu.Unlock()
	if _, err := p.Create(context.Background(), machine.CreateRequest{Name: name, Tags: []string{"ciorch"}, UserData: []byte("#cloud-config\nSECRET")}); err == nil {
		t.Fatal("应失败")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "#cloud-config\nSECRET" {
		t.Fatalf("失败应留存 user-data：err=%v content=%q", err, raw)
	}
	// 成功：删除
	bin.mu.Lock()
	bin.createErr = false
	bin.mu.Unlock()
	if _, err := p.Create(context.Background(), machine.CreateRequest{Name: name, Tags: []string{"ciorch"}, UserData: []byte("#cloud-config\nSECRET")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("成功应删 user-data：%v", err)
	}
}

// List 的 errors[] 非空（vpsctl 真实部分失败路径）必须返回 error。
func TestListPartialFailureRealPath(t *testing.T) {
	p, bin, _ := newTestProvider(t)
	bin.mu.Lock()
	bin.listErr = true
	bin.mu.Unlock()
	if ms, err := p.List(context.Background(), "ciorch"); err == nil {
		t.Fatalf("errors[] 非空必须报错，不得返回半截列表：%+v", ms)
	}
}

// ---------- Factory（池配置 "vpsctl" 子对象解析与校验，§3.3/§3.4） ----------

func factoryOpts() map[string]any {
	return map[string]any{
		"bin": "/usr/bin/vpsctl", "accounts_file": "/tmp/a.json",
		"account": "team2-ci", "region": "sgp1", "size": "c2-4vcpu-8gb",
		"image": "ubuntu-24-04-x64", "hourly_cost_hint": 0.21,
	}
}

func mustJSON(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 合法配置可构造；缺字段/坏值/未知字段报错（错误信息带字段路径）。
func TestFactoryValidation(t *testing.T) {
	env := machine.Env{StateDir: t.TempDir()}
	if _, err := Factory("do-pool", mustJSON(t, factoryOpts()), env); err != nil {
		t.Fatalf("合法配置应通过：%v", err)
	}
	for key, bad := range map[string]any{
		"bin": "", "account": "", "region": "", "size": "", "image": "",
	} {
		m := factoryOpts()
		m[key] = bad
		if _, err := Factory("do-pool", mustJSON(t, m), env); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s 缺失应报错且带字段名：%v", key, err)
		}
	}
	// region slug 形式
	m := factoryOpts()
	m["region"] = "Singapore-1"
	if _, err := Factory("do-pool", mustJSON(t, m), env); err == nil || !strings.Contains(err.Error(), "region slug") {
		t.Errorf("坏 region 应报错：%v", err)
	}
	// hourly_cost_hint ≤0 拒绝（不能当 0）
	m = factoryOpts()
	m["hourly_cost_hint"] = 0
	if _, err := Factory("do-pool", mustJSON(t, m), env); err == nil || !strings.Contains(err.Error(), "hourly_cost_hint") {
		t.Errorf("hint=0 应报错（防绕过预算）：%v", err)
	}
	// 未知字段拒绝（DisallowUnknownFields）
	m = factoryOpts()
	m["tpyo_field"] = 1
	if _, err := Factory("do-pool", mustJSON(t, m), env); err == nil {
		t.Error("拼错字段应整体拒绝")
	}
	// state_dir 缺失
	if _, err := Factory("do-pool", mustJSON(t, factoryOpts()), machine.Env{}); err == nil || !strings.Contains(err.Error(), "state_dir") {
		t.Errorf("state_dir 缺失应报错：%v", err)
	}
}

// 构造出的 Provider：CostHinter 透出 hint；prefer_private_ip 缺省= true。
func TestFactoryDefaults(t *testing.T) {
	env := machine.Env{StateDir: t.TempDir()}
	p, err := Factory("do-pool", mustJSON(t, factoryOpts()), env)
	if err != nil {
		t.Fatal(err)
	}
	vp := p.(*Provider)
	if got := vp.HourlyCostHint(); got != 0.21 {
		t.Fatalf("HourlyCostHint：%v", got)
	}
	if !vp.opt.preferPrivateIP() {
		t.Fatal("prefer_private_ip 缺省应为 true（现行行为：私网优先）")
	}
	// snapshot 非空时建机用它替代 image
	vp.opt.Snapshot = "snapshot-12345"
	if got := vp.opt.buildImage(); got != "snapshot-12345" {
		t.Fatalf("snapshot 应优先：%q", got)
	}
	// 显式 false 用公网
	f := false
	vp.opt.PreferPrivateIP = &f
	if vp.opt.preferPrivateIP() {
		t.Fatal("显式 false 应关掉私网优先")
	}
}
