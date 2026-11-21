package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/budget"

	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/githubapp"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/machine/machinetest"
	"github.com/hoshinojian/ciorch/internal/observe"
)

var tnow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// legacyNamer 复刻 vpsctl 池的命名格式（{prefix}-{account}-{region}-NN），
// 让本文件沿用的机器名字面量（ci-nms-team2-ci-sgp1-01 等）继续成立。
type legacyNamer struct{ account, region string }

func (n legacyNamer) Next(prefix string, existing []string) string {
	pre := fmt.Sprintf("%s-%s-%s-", prefix, n.account, n.region)
	max := 0
	for _, name := range existing {
		suffix, ok := strings.CutPrefix(name, pre)
		if !ok {
			continue
		}
		var num int
		if _, err := fmt.Sscanf(suffix, "%d", &num); err == nil && num > max {
			max = num
		}
	}
	return fmt.Sprintf("%s%02d", pre, max+1)
}

func (n legacyNamer) Pattern(prefix string) string {
	return fmt.Sprintf(`^%s-%s-%s-\d{2}$`, prefix, n.account, n.region)
}

// ---------- fakes ----------

type fakeObs struct{ obs observe.Obs }

func (f *fakeObs) Collect(ctx context.Context, r config.RepoEntry, p config.Profile) (observe.Obs, error) {
	return f.obs, nil
}

type fakeGH struct {
	token       string
	removeErr   map[int64]error
	removed     []int64
	listRunners []githubapp.Runner
}

func (f *fakeGH) MintToken(ctx context.Context, repo string) (string, error) {
	if f.token == "" {
		return "REGTOKEN1234567890", nil
	}
	return f.token, nil
}
func (f *fakeGH) RemoveRunner(ctx context.Context, repo string, id int64) error {
	if f.removeErr != nil {
		if err, ok := f.removeErr[id]; ok {
			return err
		}
	}
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeGH) ListRunners(ctx context.Context, repo string) ([]githubapp.Runner, error) {
	return f.listRunners, nil
}

type fakeSSH struct{ acts []string }

func (f *fakeSSH) Act(ctx context.Context, ip, action string) ([]byte, error) {
	f.acts = append(f.acts, ip+":"+action)
	return []byte(`{"ok":true}`), nil
}

type fakeRenderer struct{}

func (fakeRenderer) Render(o RenderParams) ([]byte, error) {
	if !strings.Contains(o.RepoURL, "github.com") || o.Token == "" || o.RunnerName == "" {
		return nil, fmt.Errorf("渲染参数不全: %+v", o)
	}
	return []byte("#cloud-config\n"), nil
}

// ---------- fixtures ----------

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		StateDir:          t.TempDir(),
		Global:            config.GlobalCfg{MaxMachines: 2, DailyBudgetUSD: 5},
		SSHPublicKeyFiles: []string{},
		Profiles: map[string]config.Profile{
			"go-heavy": {Pools: []string{"go-heavy"}, Labels: []string{"ci"}, RunnersPerDroplet: 1},
		},
		Repos: []config.RepoEntry{{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}},
	}
}

// newFakePool 构造一个 vpsctl 命名格式的 fake 池（池名 = profile 名，同装配层过渡桥）。
func newFakePool() *machinetest.Fake {
	f := machinetest.NewFake("go-heavy")
	f.NamerImpl = legacyNamer{account: "team2-ci", region: "sgp1"}
	f.Now = func() time.Time { return tnow }
	return f
}

func newEngine(t *testing.T, gh *fakeGH, pool *machinetest.Fake, obsrv Observer) *Engine {
	t.Helper()
	providers := map[string]machine.Provider{"go-heavy": pool}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers[r.Profile]}
	}
	e := New(testCfg(t), gh, providers, poolOf, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.SetCollector(obsrv)
	return e
}

func nmsRepo() config.RepoEntry {
	return config.RepoEntry{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}
}

func mkMachine(id, name string, age time.Duration) machine.Machine {
	return machine.Machine{Pool: "go-heavy", ID: id, Name: name, CreatedAt: tnow.Add(-age),
		Addr: "10.0.0.1", HourlyCost: 141.0 / 672.0, Tags: []string{"ciorch"}}
}

// ---------- §8.1 engine 条目 ----------

// 执行顺序：删在建前（硬寿命 Drain + 扩容 Create 同轮）。
func TestExecuteOrderDeleteBeforeCreate(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true}
	for i := 0; i < 5; i++ {
		o.Jobs = append(o.Jobs, observe.Job{ID: int64(i), RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)})
	}
	o.Machines = []machine.Machine{mkMachine("d1", name, 9*time.Hour)}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	gh := &fakeGH{}
	pool := newFakePool()
	// 预置状态：机器早已 online 且空闲 30m（硬寿命 9h → Drain；Q=5 → Create）
	st := preloadState(gh, name)
	e := newEngine(t, gh, pool, &fakeObs{o})
	e.States[nmsRepo().Repo] = st
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(pool.Events) < 2 || !strings.HasPrefix(pool.Events[0], "delete:") || !strings.HasPrefix(pool.Events[1], "create:") {
		t.Fatalf("删应在建前：events=%v", pool.Events)
	}
}

// Drain：注销失败 → 本轮不删机。
func TestDrainRegistrationFailureKeepsMachine(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true}
	o.Machines = []machine.Machine{mkMachine("d1", name, 9*time.Hour)}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	gh := &fakeGH{removeErr: map[int64]error{42: fmt.Errorf("422 Cannot delete a runner that is busy")}}
	pool := newFakePool()
	e := newEngine(t, gh, pool, &fakeObs{o})
	e.States[nmsRepo().Repo] = preloadState(gh, name)
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(pool.Deleted) != 0 {
		t.Fatalf("注销失败不应删机：deleted=%v", pool.Deleted)
	}
}

// 动作隔离：Drain 注销失败，同轮 Create 照做。
func TestActionIsolation(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true}
	for i := 0; i < 5; i++ {
		o.Jobs = append(o.Jobs, observe.Job{ID: int64(i), RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)})
	}
	o.Machines = []machine.Machine{mkMachine("d1", name, 9*time.Hour)}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	gh := &fakeGH{removeErr: map[int64]error{42: fmt.Errorf("boom")}}
	pool := newFakePool()
	e := newEngine(t, gh, pool, &fakeObs{o})
	e.States[nmsRepo().Repo] = preloadState(gh, name)
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(pool.Created) != 1 {
		t.Fatalf("一个动作失败不应拖垮同轮其他动作：created=%v", pool.Created)
	}
}

// 建机请求经 Provider：名字由池 Namer 取、cloud-init 原样传入、tags 含公共与仓标记；
// 建机失败回填退避（场景 I）。user-data 落盘/清理是 Provider 职责
// （见 internal/provider/vpsctl 的 TestUserDataLifecycle）。
func TestCreateRequestAndFailureBackoff(t *testing.T) {
	o := observe.Obs{Now: tnow, Complete: true}
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}}

	// 失败：回填 CreateFailStreak
	pool := newFakePool()
	pool.CreateErr = fmt.Errorf("DO 503")
	e := newEngine(t, &fakeGH{}, pool, &fakeObs{o})
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if e.States[nmsRepo().Repo].CreateFailStreak != 1 {
		t.Fatalf("失败应回填 CreateFailStreak=1，got %+v", e.States[nmsRepo().Repo])
	}

	// 成功：请求内容完整、名字跳号正确
	pool2 := newFakePool()
	pool2.Seed(mkMachine("d1", "ci-nms-team2-ci-sgp1-01", time.Hour))
	e2 := newEngine(t, &fakeGH{}, pool2, &fakeObs{o})
	o2 := o
	o2.Machines = []machine.Machine{mkMachine("d1", "ci-nms-team2-ci-sgp1-01", time.Hour)}
	e2.SetCollector(&fakeObs{o2})
	if _, _, err := e2.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(pool2.Created) != 1 {
		t.Fatalf("应建 1 台：%v", pool2.Created)
	}
	req := pool2.Created[0]
	if req.Name != "ci-nms-team2-ci-sgp1-02" {
		t.Fatalf("名字应跳号取 02：%q", req.Name)
	}
	if string(req.UserData) != "#cloud-config\n" || len(req.UserData) == 0 {
		t.Fatalf("cloud-init 应原样传入：%q", req.UserData)
	}
	if got := strings.Join(req.Tags, ","); !strings.Contains(got, "ciorch") || !strings.Contains(got, nmsRepo().Tag()) {
		t.Fatalf("建机 tags 应含公共与仓 tag：%v", req.Tags)
	}
}

// dry-run：只决策不执行。
func TestDryRunExecutesNothing(t *testing.T) {
	o := observe.Obs{Now: tnow, Complete: true}
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}}
	gh := &fakeGH{}
	pool := newFakePool()
	e := newEngine(t, gh, pool, &fakeObs{o})
	acts, _, err := e.Reconcile(context.Background(), nmsRepo(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) == 0 || len(pool.Created) != 0 || len(gh.removed) != 0 {
		t.Fatalf("dry-run 不执行：acts=%v created=%v", acts, pool.Created)
	}
}

// 场景 J：孤儿机器（公共标记、无仓标记）只报告，绝不删。
func TestOrphansReportedNeverDeleted(t *testing.T) {
	repoTag := nmsRepo().Tag()
	pool := newFakePool()
	pool.Seed(machine.Machine{Pool: "go-heavy", ID: "1", Name: "mystery-01", Tags: []string{"ciorch"}})                       // 孤儿
	pool.Seed(machine.Machine{Pool: "go-heavy", ID: "2", Name: "ci-nms-team2-ci-sgp1-01", Tags: []string{"ciorch", repoTag}}) // 正常
	pool.Seed(machine.Machine{Pool: "go-heavy", ID: "3", Name: "ciorch-host-1", Tags: []string{"ciorch-host"}})               // 编排器自身（公共标记列表不出）
	e := New(testCfg(t), &fakeGH{}, map[string]machine.Provider{"go-heavy": pool}, nil, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	orphans, err := e.ScanOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].ID != "1" {
		t.Fatalf("应只识别 1 台孤儿：%+v", orphans)
	}
	if len(pool.Deleted) != 0 {
		t.Fatal("孤儿绝不删除")
	}
}

// preloadState 构造"机器已 online 且空闲超 30m"的状态（硬寿命/空闲判定用）。
func preloadState(gh *fakeGH, name string) decide.RepoState {
	return decide.RepoState{
		Machines: map[string]decide.MachineState{
			name: {FirstOnlineAt: tnow.Add(-9 * time.Hour), IdleSince: tnow.Add(-30 * time.Minute)},
		},
	}
}

// 复审修正 7：多仓并发 Reconcile 不炸 map（-race 下跑）。
func TestConcurrentReposNoRace(t *testing.T) {
	cfg := testCfg(t)
	cfg.Repos = []config.RepoEntry{
		{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2},
		{Repo: "hoshinojian/web", Profile: "go-heavy", MaxRunners: 2},
	}
	// 复审复现条件：观察里有机器（Machines map 会被写），两仓并发 200 轮
	mname := "ci-nms-team2-ci-sgp1-01"
	obsrv := &fakeObs{observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true,
		Machines: []machine.Machine{mkMachine("d1", mname, 30*time.Minute)},
		Runners:  []observe.Runner{{ID: 42, Name: mname, Status: "online", Busy: false}},
		Jobs:     []observe.Job{{ID: 1, RunID: 1, Status: "in_progress", CreatedAt: tnow.Add(-20 * time.Minute), StartedAt: tnow.Add(-20 * time.Minute), RunnerName: mname}},
	}}
	pool := newFakePool()
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: pool}
	}
	e := New(cfg, &fakeGH{}, map[string]machine.Provider{"go-heavy": pool}, poolOf, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.SetCollector(obsrv)
	var wg sync.WaitGroup
	for _, r := range cfg.Repos {
		wg.Add(1)
		go func(repo config.RepoEntry) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, _, err := e.Reconcile(context.Background(), repo, true); err != nil {
					t.Errorf("reconcile: %v", err)
					return
				}
				_ = e.StatesSnapshot() // 模拟 daemon 持久化读
			}
		}(r)
	}
	wg.Wait()
}

// 复审修正 8：两仓同时要机、只剩 1 个名额 → 只建 1 台（Gate 互斥）。
func TestGateSingleSlotTwoRepos(t *testing.T) {
	cfg := testCfg(t)
	cfg.Repos = []config.RepoEntry{
		{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2},
		{Repo: "hoshinojian/web", Profile: "go-heavy", MaxRunners: 2},
	}
	// 全局已有 1 台（max=2，剩 1 名额）；list 反映 create/delete
	cl := &cluster{}
	cl.seed(machine.Machine{Pool: "go-heavy", ID: "9", Name: "ci-x-team2-ci-sgp1-01", HourlyCost: 141.0 / 672.0, Tags: []string{"ciorch"}})
	b := &budget.Budget{MaxMachines: 2, DailyBudgetUSD: 100}
	gate := budget.NewGate(b, map[string]machine.Provider{"go-heavy": cl},
		map[string]int{"go-heavy": 2}, map[string]float64{"go-heavy": 141.0 / 672.0})
	gate.Now = func() time.Time { return tnow }

	providers := map[string]machine.Provider{"go-heavy": cl}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers[r.Profile]}
	}
	e := New(cfg, &fakeGH{}, providers, poolOf, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.Gate = gate
	// 两仓并发各跑一轮（每仓 Q=3 会想建 2 台；全局只剩 1 名额）
	o := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}, {ID: 2, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}, {ID: 3, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}}
	e.SetCollector(&fakeObs{o})
	var wg sync.WaitGroup
	for _, r := range cfg.Repos {
		wg.Add(1)
		go func(repo config.RepoEntry) {
			defer wg.Done()
			if _, _, err := e.Reconcile(context.Background(), repo, false); err != nil {
				t.Errorf("reconcile: %v", err)
			}
		}(r)
	}
	wg.Wait()
	if cl.created > 1 {
		t.Fatalf("只剩 1 名额，两仓并发只允许建 1 台，got %d", cl.created)
	}
}

// 复审修正 8：预算不足 → 不建并产生场景 F 事故（QAge≥15m）。
func TestGateBudgetDenialIncidentF(t *testing.T) {
	cfg := testCfg(t)
	// 已有机器花掉了预算：1 台 141/672×2h≈0.42，日预算 0.5 → 新机 1 小时 0.21 会超
	cl := &cluster{}
	cl.seed(machine.Machine{Pool: "go-heavy", ID: "9", Name: "ci-x-team2-ci-sgp1-01", HourlyCost: 141.0 / 672.0, CreatedAt: tnow.Add(-2 * time.Hour), Tags: []string{"ciorch"}})
	b := &budget.Budget{MaxMachines: 2, DailyBudgetUSD: 0.5}
	gate := budget.NewGate(b, map[string]machine.Provider{"go-heavy": cl},
		map[string]int{"go-heavy": 2}, map[string]float64{"go-heavy": 141.0 / 672.0})
	gate.Now = func() time.Time { return tnow }

	incidents := []decide.Action{}
	providers := map[string]machine.Provider{"go-heavy": cl}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers[r.Profile]}
	}
	e := New(cfg, &fakeGH{}, providers, poolOf, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.Gate = gate
	e.OnIncident = func(ctx context.Context, repo string, a decide.Action) { incidents = append(incidents, a) }
	o := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-16 * time.Minute)}}
	e.SetCollector(&fakeObs{o})
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(incidents) == 0 || incidents[0].IncidentType != "F" {
		t.Fatalf("预算不足 + QAge≥15m 应开 F 事故：%v", incidents)
	}
}

// 复审修正 8：Drain 删机成功 → Gate 入账（SpentToday 反映）。
func TestDrainRecordsDeletionToBudget(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	cl := &cluster{}
	b := &budget.Budget{MaxMachines: 2, DailyBudgetUSD: 100}
	gate := budget.NewGate(b, map[string]machine.Provider{"go-heavy": cl},
		map[string]int{"go-heavy": 2}, map[string]float64{"go-heavy": 141.0 / 672.0})
	gate.Now = func() time.Time { return tnow }

	o := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.Machines = []machine.Machine{mkMachine("d1", name, 9*time.Hour)}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	providers := map[string]machine.Provider{"go-heavy": cl}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers[r.Profile]}
	}
	e := New(testCfg(t), &fakeGH{}, providers, poolOf, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.Gate = gate
	e.SetCollector(&fakeObs{o})
	e.States[nmsRepo().Repo] = preloadState(nil, name)
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(cl.deleted) != 1 {
		t.Fatalf("硬寿命应删机（同轮去重后恰好一次）：%v", cl.deleted)
	}
	if b.SpentToday(tnow, nil) <= 0 {
		t.Fatal("删机应入账当日花费（budget.RecordDeletion 此前无人调用）")
	}
}

// cluster 模拟一个池的全局状态：list 反映 create/delete（并发安全）——
// 现实中 provider 列表会立刻见到新机器，闸门才数得准。
type cluster struct {
	mu       sync.Mutex
	machines []machine.Machine
	created  int
	deleted  []string
}

func (c *cluster) seed(m machine.Machine) { c.machines = append(c.machines, m) }

func (c *cluster) Kind() string { return "cluster" }
func (c *cluster) Namer() machine.Namer {
	return legacyNamer{account: "team2-ci", region: "sgp1"}
}

func (c *cluster) List(ctx context.Context, tag string) ([]machine.Machine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []machine.Machine
	for _, m := range c.machines {
		for _, tg := range m.Tags {
			if tg == tag {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

func (c *cluster) Create(ctx context.Context, req machine.CreateRequest) (machine.Machine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created++
	m := machine.Machine{Pool: "go-heavy", ID: fmt.Sprintf("new-%d", c.created), Name: "ci-nms-team2-ci-sgp1-09",
		CreatedAt: tnow, HourlyCost: 141.0 / 672.0, Tags: req.Tags}
	c.machines = append(c.machines, m)
	return m, nil
}

func (c *cluster) Delete(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deleted = append(c.deleted, id)
	var out []machine.Machine
	for _, m := range c.machines {
		if m.ID != id {
			out = append(out, m)
		}
	}
	c.machines = out
	return nil
}

// 顺手修：Drain 遇 RunnerID==0（注册已不在）→ 跳过注销直接删机（否则永远
// 注销失败，机器删不掉）。
func TestDrainWithoutRegistrationDeletesDirectly(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.Machines = []machine.Machine{mkMachine("d1", name, 9*time.Hour)}
	// 无任何注册（Runners 空 → decide 的 Drain 带 RunnerID=0）
	cl := &cluster{}
	providers := map[string]machine.Provider{"go-heavy": cl}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers[r.Profile]}
	}
	e := New(testCfg(t), &fakeGH{}, providers, poolOf, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.SetCollector(&fakeObs{o})
	e.States[nmsRepo().Repo] = preloadState(nil, name)
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(cl.deleted) != 1 {
		t.Fatalf("无注册时应直接删机：%v", cl.deleted)
	}
}

// 顺手修：Reconcile 带回 Q/QAge/I/P/N（§3.9 摘要行数据源）。
func TestReconcileReturnsStats(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.Jobs = []observe.Job{
		{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-3 * time.Minute)},
		{ID: 2, RunID: 1, Status: "in_progress", CreatedAt: tnow.Add(-2 * time.Minute), StartedAt: tnow.Add(-2 * time.Minute)},
	}
	o.Machines = []machine.Machine{mkMachine("d1", name, 10*time.Minute)}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	e := newEngine(t, &fakeGH{}, newFakePool(), &fakeObs{o})
	e.States[nmsRepo().Repo] = preloadState(nil, name)
	_, stats, err := e.Reconcile(context.Background(), nmsRepo(), true)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Q != 1 || stats.QAge != 3*time.Minute || stats.I != 1 || stats.N != 1 {
		t.Fatalf("stats 不对: %+v", stats)
	}
}

// §6（实施计划）：两个池时建机/删机路由到正确的 provider、孤儿扫描覆盖所有池。
func TestTwoPoolsRouteToProviders(t *testing.T) {
	cfg := testCfg(t)
	cfg.Profiles["go-light"] = config.Profile{Pools: []string{"go-light"}, Labels: []string{"ci"}, RunnersPerDroplet: 1}
	cfg.Repos = []config.RepoEntry{
		{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2},
		{Repo: "hoshinojian/web", Profile: "go-light", MaxRunners: 2},
	}
	poolHeavy, poolLight := newFakePool(), newFakePool()
	poolLight.Pool = "go-light"
	providers := map[string]machine.Provider{"go-heavy": poolHeavy, "go-light": poolLight}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: r.Profile, Provider: providers[r.Profile]}
	}

	// nms 建机 → go-heavy；web 建机 → go-light
	oNms := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	oNms.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}}
	oWeb := oNms
	oWeb.Jobs = []observe.Job{{ID: 9, RunID: 2, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}}
	e := New(cfg, &fakeGH{}, providers, poolOf, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.SetCollector(&routeObs{byRepo: map[string]observe.Obs{"hoshinojian/nms": oNms, "hoshinojian/web": oWeb}})
	for _, r := range cfg.Repos {
		if _, _, err := e.Reconcile(context.Background(), r, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(poolHeavy.Created) != 1 || len(poolLight.Created) != 1 {
		t.Fatalf("各池应各建 1 台：heavy=%v light=%v", poolHeavy.Created, poolLight.Created)
	}

	// 删机按机器 Pool 路由：nms 的硬寿命机在 go-heavy 池
	name := "ci-nms-team2-ci-sgp1-01"
	oDrain := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	oDrain.Machines = []machine.Machine{mkMachine("d1", name, 9*time.Hour)}
	oDrain.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	e.SetCollector(&fakeObs{oDrain})
	e.States["hoshinojian/nms"] = preloadState(nil, name)
	if _, _, err := e.Reconcile(context.Background(), cfg.Repos[0], false); err != nil {
		t.Fatal(err)
	}
	if len(poolHeavy.Deleted) != 1 || len(poolLight.Deleted) != 0 {
		t.Fatalf("删机应路由到所属池：heavy=%v light=%v", poolHeavy.Deleted, poolLight.Deleted)
	}

	// 孤儿扫描覆盖所有池
	poolLight.Seed(machine.Machine{Pool: "go-light", ID: "orphan-1", Name: "mystery-02", Tags: []string{"ciorch"}})
	orphans, err := e.ScanOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range orphans {
		if m.ID == "orphan-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("孤儿扫描应覆盖全部池：%+v", orphans)
	}
}

// routeObs 按仓返回不同观察（两池路由测试用）。
type routeObs struct{ byRepo map[string]observe.Obs }

func (r *routeObs) Collect(ctx context.Context, re config.RepoEntry, p config.Profile) (observe.Obs, error) {
	return r.byRepo[re.Repo], nil
}
