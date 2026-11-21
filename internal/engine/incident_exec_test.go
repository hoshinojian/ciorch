package engine

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/budget"
	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/machine/machinetest"
	"github.com/hoshinojian/ciorch/internal/observe"
)

// dry-run：ActIncident 动作仍回调 OnIncident（shadow 观察事故），
// 但不执行任何写动作。
func TestDryRunFiresIncidentButNoActions(t *testing.T) {
	o := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.UnmatchedQueued = []observe.Job{{ID: 9, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-6 * time.Minute), Labels: []string{"nonexistent"}}}
	gh := &fakeGH{}
	pool := newFakePool()
	e := newEngine(t, gh, pool, &fakeObs{o})
	fired := []decide.Action{}
	var mu sync.Mutex
	e.OnIncident = func(ctx context.Context, repo string, a decide.Action) {
		mu.Lock()
		fired = append(fired, a)
		mu.Unlock()
	}
	acts, _, err := e.Reconcile(context.Background(), nmsRepo(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) == 0 || acts[0].Kind != decide.ActIncident {
		t.Fatalf("应产生事故动作：%v", acts)
	}
	mu.Lock()
	n := len(fired)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("dry-run 应回调 OnIncident（shadow 观察事故）：%d", n)
	}
	if len(pool.Created)+len(pool.Deleted)+len(gh.removed) != 0 {
		t.Fatal("dry-run 不得执行写动作")
	}
}

// OnRemedy：A/B/C/D 场景动作执行后回调（incident 记 jsonl 用）。
func TestOnRemedyFiredForScenarioActions(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.Machines = []machine.Machine{mkMachine("d1", name, 9*time.Hour)}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	gh := &fakeGH{}
	pool := newFakePool()
	e := newEngine(t, gh, pool, &fakeObs{o})
	e.States[nmsRepo().Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		name: {FirstOnlineAt: tnow.Add(-9 * time.Hour), IdleSince: tnow.Add(-30 * time.Minute)},
	}}
	var mu sync.Mutex
	remedies := []string{}
	e.OnRemedy = func(ctx context.Context, repo, sc, subj, detail string) {
		mu.Lock()
		remedies = append(remedies, sc+"/"+subj)
		mu.Unlock()
	}
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// 硬寿命 Drain 无 Scenario（不是 A/B/C/D 补救）→ 不应回调
	if len(remedies) != 0 {
		t.Fatalf("非场景补救不应回调 OnRemedy：%v", remedies)
	}
}

// PauseRepo：只拦建机，其余动作照常。
func TestPauseRepoSkipsCreateOnly(t *testing.T) {
	o := observe.Obs{Now: tnow, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}}
	o.Machines = []machine.Machine{mkMachine("d1", "ci-nms-team2-ci-sgp1-01", 9*time.Hour)}
	o.Runners = []observe.Runner{{ID: 42, Name: "ci-nms-team2-ci-sgp1-01", Status: "online", Busy: false}}
	gh := &fakeGH{}
	pool := newFakePool()
	e := newEngine(t, gh, pool, &fakeObs{o})
	e.Now = func() time.Time { return tnow }
	e.States[nmsRepo().Repo] = preloadState(nil, "ci-nms-team2-ci-sgp1-01")
	e.PauseRepo("hoshinojian/nms", 10*time.Minute)
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(pool.Created) != 0 {
		t.Fatal("暂停期不应建机")
	}
	if len(pool.Deleted) != 1 {
		t.Fatal("暂停不拦回收（硬寿命 Drain 照常）")
	}
	// 到期自动解除
	e.Now = func() time.Time { return tnow.Add(11 * time.Minute) }
	o2 := o
	o2.Jobs = o.Jobs
	o2.Machines = nil
	o2.Runners = nil
	e.SetCollector(&fakeObs{o2})
	if _, _, err := e.Reconcile(context.Background(), nmsRepo(), false); err != nil {
		t.Fatal(err)
	}
	if len(pool.Created) != 1 {
		t.Fatal("暂停到期后应恢复建机")
	}
}

// HumanCleared：清熔断/退避/暂停。
func TestHumanClearedResetsState(t *testing.T) {
	cfg := testCfg(t)
	e := New(cfg, nil, nil, nil, nil, nil, log.New(io.Discard, "", 0))
	e.States["o/r"] = decide.RepoState{
		Breaker:            decide.Breaker{State: "open", Fails: 3},
		CreateFailStreak:   2,
		CreateBackoffUntil: tnow.Add(time.Hour),
	}
	e.PauseRepo("o/r", time.Hour)
	e.Now = func() time.Time { return tnow }
	e.HumanCleared("o/r")
	st := e.States["o/r"]
	if st.Breaker.State != "closed" || st.Breaker.Fails != 0 || st.CreateFailStreak != 0 || !st.CreateBackoffUntil.IsZero() {
		t.Fatalf("应清熔断/退避：%+v", st)
	}
	if e.paused("o/r") {
		t.Fatal("应解除暂停")
	}
}

// RestartRunner：按观察快照的 Addr 执行 ssh；无地址报错。
func TestRestartRunnerUsesAddr(t *testing.T) {
	ssh := &fakeSSH{}
	cfg := testCfg(t)
	providers := map[string]machine.Provider{}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool { return observe.Pool{} }
	_ = poolOf
	e := New(cfg, &fakeGH{}, providers, nil, ssh, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.noteObs("hoshinojian/nms", observe.Obs{Machines: []machine.Machine{mkMachine("d1", "ci-nms-team2-ci-sgp1-01", time.Hour)}})
	if err := e.RestartRunner(context.Background(), "hoshinojian/nms", "ci-nms-team2-ci-sgp1-01"); err != nil {
		t.Fatal(err)
	}
	if len(ssh.acts) != 1 || ssh.acts[0] != "10.0.0.1:restart-runner" {
		t.Fatalf("ssh 目标与动作：%v", ssh.acts)
	}
	if err := e.RestartRunner(context.Background(), "hoshinojian/nms", "no-such"); err == nil {
		t.Fatal("无地址应报错")
	}
}

// DrainMachine（事故动作路径）：注销 + 按池删机 + 入账。
func TestDrainMachineAsAction(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	cl := &cluster{}
	b := &budget.Budget{MaxMachines: 2, DailyBudgetUSD: 100}
	gate := budget.NewGate(b, map[string]machine.Provider{"go-heavy": cl},
		map[string]int{"go-heavy": 2}, map[string]float64{"go-heavy": 141.0 / 672.0})
	gate.Now = func() time.Time { return tnow }
	providers := map[string]machine.Provider{"go-heavy": cl}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers["go-heavy"]}
	}
	_ = poolOf
	e := New(testCfg(t), &fakeGH{}, providers, nil, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.Gate = gate
	e.noteObs("hoshinojian/nms", observe.Obs{Now: tnow, Machines: []machine.Machine{mkMachine("d1", name, time.Hour)}})
	if err := e.DrainMachine(context.Background(), "hoshinojian/nms", name); err != nil {
		t.Fatal(err)
	}
	if len(cl.deleted) != 1 {
		t.Fatalf("应删机：%v", cl.deleted)
	}
	if b.SpentToday(tnow, nil) <= 0 {
		t.Fatal("应入账")
	}
}

// ReplaceMachine：有配额先建后拆；无配额降级 drain 后再建。
func TestReplaceMachineQuotaFallback(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	// 池满（cap=1 且已被旧机占用）→ 第一次建被拒 → drain 旧机 → 再建成功
	cl := &cluster{}
	cl.seed(machine.Machine{Pool: "go-heavy", ID: "9", Name: name, HourlyCost: 141.0 / 672.0, Tags: []string{"ciorch"}})
	b := &budget.Budget{MaxMachines: 2, DailyBudgetUSD: 100}
	gate := budget.NewGate(b, map[string]machine.Provider{"go-heavy": cl},
		map[string]int{"go-heavy": 1}, map[string]float64{"go-heavy": 141.0 / 672.0})
	gate.Now = func() time.Time { return tnow }

	providers := map[string]machine.Provider{"go-heavy": cl}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "go-heavy", Provider: providers["go-heavy"]}
	}
	_ = poolOf
	e := New(testCfg(t), &fakeGH{}, providers, poolOf, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.Gate = gate
	e.noteObs("hoshinojian/nms", observe.Obs{Now: tnow, Machines: []machine.Machine{mkMachine("9", name, time.Hour)}})
	if err := e.ReplaceMachine(context.Background(), "hoshinojian/nms", name); err != nil {
		t.Fatal(err)
	}
	if len(cl.deleted) != 1 || cl.created < 1 {
		t.Fatalf("无配额应降级 drain 后再建：deleted=%v created=%d", cl.deleted, cl.created)
	}
}

// SwitchPool 覆写：poolFor 返回覆写池且到期失效。
func TestSwitchPoolOverride(t *testing.T) {
	cfg := testCfg(t)
	base := machinetest.NewFake("do-sgp1-c2")
	alt := machinetest.NewFake("local-incus")
	providers := map[string]machine.Provider{"do-sgp1-c2": base, "local-incus": alt}
	poolOf := func(r config.RepoEntry, p config.Profile) observe.Pool {
		return observe.Pool{Name: "do-sgp1-c2", Provider: base}
	}
	_ = poolOf
	e := New(cfg, nil, providers, poolOf, nil, nil, log.New(io.Discard, "", 0))
	e.Now = func() time.Time { return tnow }
	re := config.RepoEntry{Repo: "hoshinojian/nms", Profile: "go-heavy"}
	if got := e.poolFor(re, cfg.Profiles["go-heavy"]).Name; got != "do-sgp1-c2" {
		t.Fatalf("默认池：%q", got)
	}
	e.SwitchPool("hoshinojian/nms", "local-incus", 6*time.Hour)
	if got := e.poolFor(re, cfg.Profiles["go-heavy"]).Name; got != "local-incus" {
		t.Fatalf("覆写池：%q", got)
	}
	e.Now = func() time.Time { return tnow.Add(7 * time.Hour) }
	if got := e.poolFor(re, cfg.Profiles["go-heavy"]).Name; got != "do-sgp1-c2" {
		t.Fatalf("到期应回默认池：%q", got)
	}
}

// RerunFailedJobs 动作直达 GH。
func TestRerunAction(t *testing.T) {
	gh := &fakeGH{}
	e := New(testCfg(t), gh, nil, nil, nil, nil, log.New(io.Discard, "", 0))
	if err := e.RerunFailedJobs(context.Background(), "hoshinojian/nms", 4242); err != nil {
		t.Fatal(err)
	}
	if len(gh.reruns) != 1 || gh.reruns[0] != 4242 {
		t.Fatalf("rerun：%v", gh.reruns)
	}
}
