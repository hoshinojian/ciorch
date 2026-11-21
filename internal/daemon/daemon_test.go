package daemon

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/budget"
	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/engine"
	"github.com/hoshinojian/ciorch/internal/githubapp"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/machine/machinetest"
	"github.com/hoshinojian/ciorch/internal/observe"
)

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	cfg := &config.Config{
		StateDir: t.TempDir(),
		Global:   config.GlobalCfg{MaxDroplets: 2, DailyBudgetUSD: 5},
		Profiles: map[string]config.Profile{
			"go-heavy": {Account: "team2-ci", Region: "sgp1", RunnersPerDroplet: 1},
		},
		Repos: []config.RepoEntry{{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}},
	}
	// 本文件只测间隔切换/触发合并/摘要，不触 IO 面——接口传 nil。
	e := engine.New(cfg, nil, nil, nil, nil, nil, log.New(io.Discard, "", 0))
	return New(cfg, e, &budget.Budget{MaxDroplets: 2, DailyBudgetUSD: 5}, log.New(io.Discard, "", 0))
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
		Global:   config.GlobalCfg{MaxDroplets: 2, DailyBudgetUSD: 5},
		Profiles: map[string]config.Profile{
			"go-heavy": {Account: "team2-ci", Region: "sgp1", RunnersPerDroplet: 1},
		},
		Repos: []config.RepoEntry{{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}},
	}
	providers := map[string]machine.Provider{"go-heavy": pool}
	poolOf := func(r config.RepoEntry, p config.Profile) machine.Provider { return providers[r.Profile] }
	e := engine.New(cfg, gh, providers, poolOf, ssh, nil, log.New(io.Discard, "", 0))
	e.SetCollector(&fixedObs{o})
	d := New(cfg, e, &budget.Budget{MaxDroplets: 2, DailyBudgetUSD: 5}, log.New(io.Discard, "", 0))
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

type recSSH struct{ acts []string }

func (f *recSSH) Act(ctx context.Context, ip, action string) ([]byte, error) {
	f.acts = append(f.acts, ip+":"+action)
	return nil, nil
}
