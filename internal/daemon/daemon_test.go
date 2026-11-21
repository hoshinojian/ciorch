package daemon

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/budget"
	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/engine"
	"github.com/hoshinojian/ciorch/internal/githubapp"
	"github.com/hoshinojian/ciorch/internal/incident"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/machine/machinetest"
	"github.com/hoshinojian/ciorch/internal/observe"
)

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	cfg := &config.Config{
		StateDir: t.TempDir(),
		Global:   config.GlobalCfg{MaxMachines: 2, DailyBudgetUSD: 5},
		Profiles: map[string]config.Profile{
			"go-heavy": {Pools: []string{"go-heavy"}, RunnersPerDroplet: 1},
		},
		Repos: []config.RepoEntry{{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}},
	}
	// 本文件只测间隔切换/触发合并/摘要，不触 IO 面——接口传 nil。
	e := engine.New(cfg, nil, nil, nil, nil, nil, log.New(io.Discard, "", 0))
	return New(cfg, e, &budget.Budget{MaxMachines: 2, DailyBudgetUSD: 5}, log.New(io.Discard, "", 0))
}

// global.tuning 的 poll_active/poll_idle 覆盖默认轮询间隔（§3.6.6；
// 2026-11-21 冒烟发现 tuning 此前是死配置，本测守护接线）。
func TestNewAppliesPollTuning(t *testing.T) {
	d := testDaemon(t)
	d.Cfg.Global.Tuning = map[string]string{"poll_active": "5s", "poll_idle": "30s"}
	d2 := New(d.Cfg, d.Engine, d.Budget, log.New(io.Discard, "", 0))
	if d2.PollActive != 5*time.Second || d2.PollIdle != 30*time.Second {
		t.Fatalf("poll 覆盖未生效: %v/%v", d2.PollActive, d2.PollIdle)
	}
	// 非法值保默认（不 panic、不清零）
	d.Cfg.Global.Tuning = map[string]string{"poll_active": "nope"}
	d3 := New(d.Cfg, d.Engine, d.Budget, log.New(io.Discard, "", 0))
	if d3.PollActive != defaultPollActive {
		t.Fatalf("非法值应保默认: %v", d3.PollActive)
	}
}

// 间隔切换（§3.6.6）：空闲 → poll_idle；冷启在途/在忙/补救中 → poll_active。
func TestNextIntervalSwitching(t *testing.T) {
	d := testDaemon(t)
	d.PollActive, d.PollIdle = 15*time.Second, 60*time.Second
	repo := d.Cfg.Repos[0]

	if got := d.nextInterval(repo, time.Now()); got != 60*time.Second {
		t.Fatalf("无状态应空闲档，got %v", got)
	}
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstSeen: time.Now()}, // 冷启在途（FirstOnlineAt 零值）
	}}
	if got := d.nextInterval(repo, time.Now()); got != 15*time.Second {
		t.Fatalf("冷启在途应活跃档，got %v", got)
	}
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstOnlineAt: time.Now(), BusySince: time.Now()},
	}}
	if got := d.nextInterval(repo, time.Now()); got != 15*time.Second {
		t.Fatalf("busy 应活跃档，got %v", got)
	}
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstOnlineAt: time.Now(), Remedy: "restarting", RemedyAt: time.Now()},
	}}
	if got := d.nextInterval(repo, time.Now()); got != 15*time.Second {
		t.Fatalf("补救中应活跃档，got %v", got)
	}
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstOnlineAt: time.Now(), IdleSince: time.Now()},
	}}
	if got := d.nextInterval(repo, time.Now()); got != 60*time.Second {
		t.Fatalf("在线空闲应空闲档，got %v", got)
	}
}

// Trigger 合并：channel 满时再触发不阻塞。
func TestTriggerNonBlocking(t *testing.T) {
	d := testDaemon(t)
	ch := make(chan struct{}, 1)
	repo := d.Cfg.Repos[0].Repo
	d.mu.Lock()
	d.triggers[repo] = ch
	d.mu.Unlock()
	d.Trigger(repo)
	d.Trigger(repo) // 第二次应合并而非阻塞
	select {
	case <-ch:
	default:
		t.Fatal("应有一次触发入队")
	}
}

// kindsOf：动作名列表（空 → 无）。
func TestKindsOf(t *testing.T) {
	if got := kindsOf(nil); got != "无" {
		t.Fatalf("空动作应显示 无，got %q", got)
	}
	if got := kindsOf([]decide.Action{{Kind: decide.ActDrain}, {Kind: decide.ActCreate}}); got != "drain,create" {
		t.Fatalf("动作摘要不对: %q", got)
	}
}

// active() 考虑 Q>0（复审修正：此前只看机器状态）。
func TestActiveWithQueue(t *testing.T) {
	d := testDaemon(t)
	repo := d.Cfg.Repos[0]
	d.mu.Lock()
	d.lastStats[repo.Repo] = engine.RoundStats{Q: 1}
	d.mu.Unlock()
	if !d.active(repo) {
		t.Fatal("有排队应活跃档")
	}
	d.mu.Lock()
	d.lastStats[repo.Repo] = engine.RoundStats{}
	d.mu.Unlock()
	if d.active(repo) {
		t.Fatal("无排队无机器应空闲档")
	}
}

// 审查修正 1：daemon -dry-run 必须零写调用（曾有会触发删机的输入仍真执行）。
// 用带记录的 fake GH/VPS/SSH 跑一轮 round()，输入含硬寿命到期的机器。
func TestDaemonDryRunZeroWrites(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: time.Now(), Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.Machines = []machine.Machine{{Pool: "go-heavy", ID: "d1", Name: name, CreatedAt: time.Now().Add(-9 * time.Hour), HourlyCost: 141.0 / 672.0}}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: time.Now().Add(-time.Minute)}}

	gh := &recGH{}
	pool := machinetest.NewFake("go-heavy")
	ssh := &recSSH{}
	cfg := &config.Config{
		StateDir: t.TempDir(),
		Global:   config.GlobalCfg{MaxMachines: 2, DailyBudgetUSD: 5},
		Profiles: map[string]config.Profile{
			"go-heavy": {Pools: []string{"go-heavy"}, RunnersPerDroplet: 1},
		},
		Repos: []config.RepoEntry{{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}},
	}
	providers := map[string]machine.Provider{"go-heavy": pool}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers[r.Profile]}
	}
	e := engine.New(cfg, gh, providers, poolOf, ssh, nil, log.New(io.Discard, "", 0))
	e.SetCollector(&fixedObs{o})
	d := New(cfg, e, &budget.Budget{MaxMachines: 2, DailyBudgetUSD: 5}, log.New(io.Discard, "", 0))
	d.DryRun = true
	next := d.round(context.Background(), cfg.Repos[0], filepath.Join(cfg.StateDir, "state.json"))
	if next != d.PollActive {
		t.Fatalf("有排队应活跃档，got %v", next)
	}
	if len(gh.removed)+len(pool.Created)+len(pool.Deleted)+len(ssh.acts) != 0 {
		t.Fatalf("dry-run 必须零写调用：removed=%v created=%v deleted=%v ssh=%v",
			gh.removed, pool.Created, pool.Deleted, ssh.acts)
	}
}

type fixedObs struct{ obs observe.Obs }

func (f *fixedObs) Collect(ctx context.Context, r config.RepoEntry, p config.Profile) (observe.Obs, error) {
	return f.obs, nil
}

type recGH struct {
	removed []int64
}

func (f *recGH) MintToken(ctx context.Context, repo string) (string, error) { return "T", nil }
func (f *recGH) RemoveRunner(ctx context.Context, repo string, id int64) error {
	f.removed = append(f.removed, id)
	return nil
}
func (f *recGH) ListRunners(ctx context.Context, repo string) ([]githubapp.Runner, error) {
	return nil, nil
}
func (f *recGH) RerunFailedJobs(ctx context.Context, repo string, runID int64) error {
	return nil
}

type recSSH struct{ acts []string }

func (f *recSSH) Act(ctx context.Context, ip, action string) ([]byte, error) {
	f.acts = append(f.acts, ip+":"+action)
	return nil, nil
}

// 阶段 5 集成：daemon 一轮（dry-run）→ 引擎事故动作 → incident.Manager（DryRun）
// 只记 jsonl/日志，零 issue 副作用；时间线有观察摘要。
func TestDaemonDryRunIncidentNoSideEffects(t *testing.T) {
	o := observe.Obs{Now: time.Now(), Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.UnmatchedQueued = []observe.Job{{ID: 9, RunID: 1, Status: "queued", CreatedAt: time.Now().Add(-6 * time.Minute), Labels: []string{"nonexistent"}}}

	gh := &recGH{}
	pool := machinetest.NewFake("go-heavy")
	ssh := &recSSH{}
	cfg := &config.Config{
		StateDir: t.TempDir(),
		Global:   config.GlobalCfg{MaxMachines: 2, DailyBudgetUSD: 5},
		Profiles: map[string]config.Profile{
			"go-heavy": {Pools: []string{"go-heavy"}, RunnersPerDroplet: 1},
		},
		Repos: []config.RepoEntry{{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}},
	}
	providers := map[string]machine.Provider{"go-heavy": pool}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers[r.Profile]}
	}
	e := engine.New(cfg, gh, providers, poolOf, ssh, nil, log.New(io.Discard, "", 0))
	e.SetCollector(&fixedObs{o})

	issues := &fakeIncIssues{}
	stateDir := t.TempDir()
	mgr := incident.New(issues, e, incident.Options{MaintainerLogin: "maint"}, stateDir, log.New(io.Discard, "", 0))
	mgr.DryRun = true
	mgr.RestoreLLM(time.Now().UTC().Format("2006-01-02"), 1) // 预置日额：证明 llm_day 字段落盘
	e.OnIncident = func(ctx context.Context, repo string, a decide.Action) {
		_ = mgr.Handle(ctx, repo, a)
	}

	d := New(cfg, e, &budget.Budget{MaxMachines: 2, DailyBudgetUSD: 5}, log.New(io.Discard, "", 0))
	d.DryRun = true
	d.Incidents = mgr
	// 走 daemon 的 round（而非裸 Reconcile）：把摘要喂进时间线、推进 Tick、
	// 并把事故表持久化进 state.json（证明重启后关 issue 找得到事故）
	statePath := filepath.Join(cfg.StateDir, "state.json")
	if _, _, err := e.Reconcile(context.Background(), cfg.Repos[0], true); err != nil {
		t.Fatal(err)
	}
	d.round(context.Background(), cfg.Repos[0], statePath)
	mgr.Note(cfg.Repos[0].Repo, "observe", "Q=0")
	mgr.Tick(context.Background(), cfg.Repos[0].Repo)

	if issues.created+issues.comments+issues.closed+issues.reopened+issues.ensured != 0 {
		t.Fatalf("dry-run 事故模块必须零 issue 副作用：%+v", issues)
	}
	if len(pool.Created)+len(pool.Deleted)+len(ssh.acts) != 0 {
		t.Fatal("dry-run 引擎必须零写动作")
	}
	// jsonl 已记（detected）
	raw, err := os.ReadFile(filepath.Join(stateDir, "incidents.jsonl"))
	if err != nil || !strings.Contains(string(raw), "\"event\":\"detected\"") {
		t.Fatalf("dry-run 应记 incidents.jsonl：%v %s", err, raw)
	}
	// state.json 含事故表与 LLM 日额（§3.12：重启恢复）
	sj, err := os.ReadFile(statePath)
	if err != nil || !strings.Contains(string(sj), "\"incidents\"") || !strings.Contains(string(sj), "\"llm_day\"") {
		t.Fatalf("state.json 应持久化事故表与 LLM 日额：%v %s", err, sj)
	}
}

// fakeIncIssues：零容忍的假 issue API（任何调用都算副作用）。
type fakeIncIssues struct {
	created, comments, closed, reopened, ensured int
}

func (f *fakeIncIssues) EnsureLabels(ctx context.Context, repo string, labels []string) error {
	f.ensured++
	return nil
}
func (f *fakeIncIssues) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (githubapp.Issue, error) {
	f.created++
	return githubapp.Issue{Number: 1}, nil
}
func (f *fakeIncIssues) AddIssueComment(ctx context.Context, repo string, number int64, body string) error {
	f.comments++
	return nil
}
func (f *fakeIncIssues) AddIssueLabels(ctx context.Context, repo string, number int64, labels []string) error {
	return nil
}
func (f *fakeIncIssues) CloseIssue(ctx context.Context, repo string, number int64) error {
	f.closed++
	return nil
}
func (f *fakeIncIssues) ReopenIssue(ctx context.Context, repo string, number int64) error {
	f.reopened++
	return nil
}
