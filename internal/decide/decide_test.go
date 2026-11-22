package decide

import (
	"regexp"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/observe"
)

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

var ownedRe = regexp.MustCompile(`^ci-nms-team2-ci-sgp1-\d{2}(-a|-b)?$`)

func params() Params {
	return Params{MaxRunners: 2, RunnersPerDroplet: 1, OwnedRunnerRe: ownedRe}
}

func obs() observe.Obs {
	return observe.Obs{Now: t0, Complete: true, JobsOK: true, RunnersOK: true, MachinesOK: true}
}

func droplet(name string, age time.Duration) machine.Machine {
	return machine.Machine{ID: "d-" + name, Name: name, CreatedAt: t0.Add(-age)}
}

func onlineRunner(name string, busy bool) observe.Runner {
	return observe.Runner{ID: int64(len(name)), Name: name, Status: "online", Busy: busy}
}

func queuedJob(id int64, age time.Duration) observe.Job {
	return observe.Job{ID: id, RunID: 100, Status: "queued", CreatedAt: t0.Add(-age)}
}

func countKind(acts []Action, k ActionKind) int {
	n := 0
	for _, a := range acts {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func hasActionOn(acts []Action, k ActionKind, name string) bool {
	for _, a := range acts {
		if a.Kind == k && a.DropletName == name {
			return true
		}
	}
	return false
}

func hasIncident(acts []Action, typ string) bool {
	for _, a := range acts {
		if a.Kind == ActIncident && a.IncidentType == typ {
			return true
		}
	}
	return false
}

// mutate 取出-改-放回（Go 不允许直接给 map 内结构体字段赋值）。
func mutate(st *RepoState, name string, f func(*MachineState)) {
	ms := st.Machines[name]
	f(&ms)
	st.Machines[name] = ms
}

// 机器已在线（FirstOnlineAt 已置，避免落入冷启/掉线判定）。
func stateWithOnline(name string) RepoState {
	return RepoState{Machines: map[string]MachineState{
		name: {FirstOnlineAt: t0.Add(-10 * time.Minute)},
	}}
}

// ---------- 扩容（§8.1）----------

// Q=7、N=0、max=2 → 2 个 Create。
func TestScaleUpTwoCreates(t *testing.T) {
	o := obs()
	for i := 0; i < 7; i++ {
		o.Jobs = append(o.Jobs, queuedJob(int64(i), time.Minute))
	}
	acts, _ := Decide(params(), RepoState{}, o)
	if n := countKind(acts, ActCreate); n != 2 {
		t.Fatalf("Q=7 N=0 max=2 应建 2 台，got %d（%v）", n, acts)
	}
}

// Q=7、P=1（一台冷启在途）→ 1 个。
func TestScaleUpWithPending(t *testing.T) {
	o := obs()
	for i := 0; i < 7; i++ {
		o.Jobs = append(o.Jobs, queuedJob(int64(i), time.Minute))
	}
	o.Machines = []machine.Machine{droplet("ci-nms-team2-ci-sgp1-01", 1*time.Minute)} // 冷启中
	acts, _ := Decide(params(), RepoState{}, o)
	if n := countKind(acts, ActCreate); n != 1 {
		t.Fatalf("Q=7 P=1 应再建 1 台，got %d（%v）", n, acts)
	}
}

// Q=1、I=1（已有空闲 runner）→ 0 个。
func TestScaleUpSatisfiedByIdle(t *testing.T) {
	o := obs()
	o.Jobs = []observe.Job{queuedJob(1, time.Minute)}
	o.Machines = []machine.Machine{droplet("ci-nms-team2-ci-sgp1-01", 10*time.Minute)}
	o.Runners = []observe.Runner{onlineRunner("ci-nms-team2-ci-sgp1-01", false)}
	acts, _ := Decide(params(), stateWithOnline("ci-nms-team2-ci-sgp1-01"), o)
	if n := countKind(acts, ActCreate); n != 0 {
		t.Fatalf("Q=1 I=1 不应再建，got %d（%v）", n, acts)
	}
}

// 原型缺陷 #2 回归：in_progress run 内有 queued job、无 droplet → 必须 Create。
func TestPrototypeBug2Regression(t *testing.T) {
	o := obs()
	o.Jobs = []observe.Job{
		{ID: 2, RunID: 100, Status: "queued", CreatedAt: t0.Add(-3 * time.Minute)},
		{ID: 1, RunID: 100, Status: "in_progress", CreatedAt: t0.Add(-10 * time.Minute)},
	}
	acts, _ := Decide(params(), RepoState{}, o)
	if n := countKind(acts, ActCreate); n != 1 {
		t.Fatalf("无 droplet 有排队 job 必须建机（原型 #2 回归），got %d", n)
	}
}

// ---------- 缩容（§8.1）----------

// 空闲 19m 不删、21m 删。
func TestIdleTeardownThreshold(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Machines = []machine.Machine{droplet(name, 30*time.Minute)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	st := RepoState{Machines: map[string]MachineState{name: {
		FirstOnlineAt: t0.Add(-30 * time.Minute), IdleSince: t0.Add(-19 * time.Minute),
	}}}
	if acts, _ := Decide(params(), st, o); countKind(acts, ActDrain) != 0 {
		t.Fatal("空闲 19m 不应删")
	}
	st.Machines[name] = MachineState{FirstOnlineAt: t0.Add(-30 * time.Minute), IdleSince: t0.Add(-21 * time.Minute)}
	if acts, _ := Decide(params(), st, o); !hasActionOn(acts, ActDrain, name) {
		t.Fatalf("空闲 21m 应删（%v）", acts)
	}
}

// 近 2h 有 2 次活动 → 用 busy 档：44m 不删、46m 删。
func TestIdleTimeoutAdaptive(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Machines = []machine.Machine{droplet(name, time.Hour)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	base := RepoState{
		Machines:     map[string]MachineState{name: {FirstOnlineAt: t0.Add(-time.Hour)}},
		LastActivity: []time.Time{t0.Add(-30 * time.Minute), t0.Add(-50 * time.Minute)},
	}
	for _, c := range []struct {
		idle time.Duration
		want bool
	}{
		{44 * time.Minute, false},
		{46 * time.Minute, true},
	} {
		st := base
		st.Machines[name] = MachineState{FirstOnlineAt: t0.Add(-time.Hour), IdleSince: t0.Add(-c.idle)}
		acts, _ := Decide(params(), st, o)
		if got := hasActionOn(acts, ActDrain, name); got != c.want {
			t.Fatalf("活跃期 idle=%v：want 删=%v got=%v（%v）", c.idle, c.want, got, acts)
		}
	}
}

// Q>0 不删；Complete=false 不删。
func TestNoTeardownWhenQueuedOrIncomplete(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	st := RepoState{Machines: map[string]MachineState{name: {
		FirstOnlineAt: t0.Add(-time.Hour), IdleSince: t0.Add(-30 * time.Minute),
	}}}
	// Q>0
	o := obs()
	o.Jobs = []observe.Job{queuedJob(1, time.Minute)}
	o.Machines = []machine.Machine{droplet(name, time.Hour)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	if acts, _ := Decide(params(), st, o); countKind(acts, ActDrain) != 0 {
		t.Fatal("Q>0 绝不缩容")
	}
	// 观察不完整
	o2 := obs()
	o2.Complete = false
	o2.Machines = o.Machines
	o2.Runners = o.Runners
	if acts, _ := Decide(params(), st, o2); countKind(acts, ActDrain) != 0 {
		t.Fatal("Complete=false 不缩容")
	}
}

// 两台同时到期：一轮只删一台（最老的）。
func TestTeardownOnePerRound(t *testing.T) {
	old, young := "ci-nms-team2-ci-sgp1-01", "ci-nms-team2-ci-sgp1-02"
	o := obs()
	o.Machines = []machine.Machine{droplet(old, 2*time.Hour), droplet(young, time.Hour)}
	o.Runners = []observe.Runner{onlineRunner(old, false), onlineRunner(young, false)}
	st := RepoState{Machines: map[string]MachineState{
		old:   {FirstOnlineAt: t0.Add(-2 * time.Hour), IdleSince: t0.Add(-40 * time.Minute)},
		young: {FirstOnlineAt: t0.Add(-time.Hour), IdleSince: t0.Add(-39 * time.Minute)},
	}}
	acts, _ := Decide(params(), st, o)
	if n := countKind(acts, ActDrain); n != 1 {
		t.Fatalf("一轮只回收一台，got %d", n)
	}
	if !hasActionOn(acts, ActDrain, old) {
		t.Fatalf("应回收最老的一台（%v）", acts)
	}
}

// ---------- 硬寿命（§8.1）----------

func TestHardLifetime(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	// 8h 空闲 → 删（观察不完整也删——但 droplets 路必须成功）
	o := obs()
	o.Complete = false
	o.JobsOK = false
	o.Machines = []machine.Machine{droplet(name, 8*time.Hour+time.Minute)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	st := RepoState{Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-8 * time.Hour), IdleSince: t0.Add(-time.Hour)}}}
	if acts, _ := Decide(params(), st, o); !hasActionOn(acts, ActDrain, name) {
		t.Fatalf("8h 空闲应删（Complete=false 也执行）：%v", acts)
	}
	// 8h 但在忙 → 不删，未到 10h 不告警
	o2 := obs()
	o2.Machines = []machine.Machine{droplet(name, 8*time.Hour+time.Minute)}
	o2.Runners = []observe.Runner{onlineRunner(name, true)}
	st2 := RepoState{Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-8 * time.Hour), BusySince: t0.Add(-30 * time.Minute)}}}
	acts, _ := Decide(params(), st2, o2)
	if hasActionOn(acts, ActDrain, name) {
		t.Fatal("硬寿命到期但机器在忙不删")
	}
	if hasIncident(acts, "D") {
		t.Fatal("8h+1m 在忙未到 +2h 告警线")
	}
	// ≥10h 无论忙闲 → 告警（场景 D 变体）
	o3 := obs()
	o3.Machines = []machine.Machine{droplet(name, 10*time.Hour+time.Minute)}
	o3.Runners = []observe.Runner{onlineRunner(name, true)}
	st3 := RepoState{Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-10 * time.Hour), BusySince: t0.Add(-3 * time.Hour)}}}
	if acts, _ := Decide(params(), st3, o3); !hasIncident(acts, "D") {
		t.Fatalf("硬寿命+2h 仍忙应告警：%v", acts)
	}
}

// ---------- 场景 A 假活（§8.1）----------

func aFixture() (observe.Obs, RepoState) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Jobs = []observe.Job{queuedJob(1, 3*time.Minute)} // QAge=3m ≥ 2m
	o.Machines = []machine.Machine{droplet(name, time.Hour)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	st := RepoState{Machines: map[string]MachineState{name: {
		FirstOnlineAt: t0.Add(-5 * time.Minute), // 上线超 60s
		IdleSince:     t0.Add(-3 * time.Minute), // 本机持续空闲 ≥ 2m
	}}}
	return o, st
}

// 线上误判 1（2026-11-22 17:00，nms run 45046644413）：runners 接口读到 busy=false，
// 但 jobs 接口显示本机 runner 正在跑 in_progress job → 不判假活（job 佐证优先）。
func TestStallNotWhenJobAssigned(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	mutate(&st, name, func(m *MachineState) { m.StallSamples = 2 })
	o.Jobs = append(o.Jobs, observe.Job{ID: 9, RunID: 100, Status: "in_progress",
		CreatedAt: t0.Add(-3 * time.Minute), StartedAt: t0.Add(-3 * time.Minute), RunnerName: name})
	acts, st2 := Decide(params(), st, o)
	if countKind(acts, ActSSHAct) != 0 || hasIncident(acts, "A") {
		t.Fatalf("本机有 in_progress job 不应判假活：%v", acts)
	}
	if st2.Machines[name].StallSamples != 0 {
		t.Fatalf("有 job 佐证应清零采样：%+v", st2.Machines[name])
	}
}

// 线上误判 2（2026-11-22 16:44，nms run 45044354990）：满载排队 12m 后本机刚跑完
// 上一个 job、空闲才 55s（交接间隙，77s 后自己接到下一个）→ QAge 再老也不判假活。
func TestStallNeedsMachineIdleDuration(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	o.Jobs = []observe.Job{queuedJob(1, 12*time.Minute)}
	mutate(&st, name, func(m *MachineState) {
		m.StallSamples = 2
		m.IdleSince = t0.Add(-55 * time.Second)
	})
	if acts, _ := Decide(params(), st, o); countKind(acts, ActSSHAct) != 0 {
		t.Fatalf("本机空闲不足 2m 不应判假活：%v", acts)
	}
}

// 重启观察窗内：busy 仍读到 false 但 jobs 显示已接活 → 同样解除补救，不判死。
func TestStallRestartGraceRecoveryByJob(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	st.Machines[name] = MachineState{
		FirstOnlineAt: t0.Add(-10 * time.Minute), Remedy: "restarting", RemedyAt: t0.Add(-3 * time.Minute),
	}
	o.Jobs = append(o.Jobs, observe.Job{ID: 9, RunID: 100, Status: "in_progress",
		CreatedAt: t0.Add(-time.Minute), StartedAt: t0.Add(-time.Minute), RunnerName: name})
	acts, st2 := Decide(params(), st, o)
	if st2.Machines[name].Bad || hasActionOn(acts, ActDrain, name) || st2.Machines[name].Remedy != "" {
		t.Fatalf("jobs 显示已接活应解除补救而非判死：%v %+v", acts, st2.Machines[name])
	}
}

// 只差一轮不触发（此前 1 次 + 本轮 = 2 次 < 3）。
func TestStallNeedsThreeSamples(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	mutate(&st, name, func(m *MachineState) { m.StallSamples = 1 })
	acts, _ := Decide(params(), st, o)
	if countKind(acts, ActSSHAct) != 0 || countKind(acts, ActCreate) != 0 {
		t.Fatalf("只差一轮不应触发：%v", acts)
	}
}

// 3 轮触发 → Create + SSHAct。
func TestStallTriggersReplaceAndRestart(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	mutate(&st, name, func(m *MachineState) { m.StallSamples = 2 }) // 本轮第 3 次
	acts, st2 := Decide(params(), st, o)
	if countKind(acts, ActCreate) != 1 || countKind(acts, ActSSHAct) != 1 {
		t.Fatalf("3 轮命中应 Create+SSHAct：%v", acts)
	}
	if st2.Machines[name].Remedy != "restarting" {
		t.Fatalf("应记录 restarting 补救：%+v", st2.Machines[name])
	}
}

// 重启后 2m 内 busy → 解除。
func TestStallRestartGraceRecovery(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	st.Machines[name] = MachineState{
		FirstOnlineAt: t0.Add(-5 * time.Minute), Remedy: "restarting", RemedyAt: t0.Add(-1 * time.Minute),
	}
	o.Runners = []observe.Runner{onlineRunner(name, true)} // 接到活了
	_, st2 := Decide(params(), st, o)
	if st2.Machines[name].Remedy != "" || st2.Machines[name].StallSamples != 0 {
		t.Fatalf("重启后接到活应解除补救：%+v", st2.Machines[name])
	}
}

// 重启后 2m 仍不接 → Bad + Drain。
func TestStallRestartGraceExpired(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	st.Machines[name] = MachineState{
		FirstOnlineAt: t0.Add(-10 * time.Minute), Remedy: "restarting", RemedyAt: t0.Add(-3 * time.Minute),
	}
	acts, st2 := Decide(params(), st, o)
	if !st2.Machines[name].Bad || !hasActionOn(acts, ActDrain, name) {
		t.Fatalf("观察窗后仍不接活应判死回收：%v %+v", acts, st2.Machines[name])
	}
}

// 上线不足 60s 不判；QAge 不足不判。
func TestStallGuards(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	// 上线仅 30s：即使第 3 轮也不触发
	o, st := aFixture()
	mutate(&st, name, func(m *MachineState) { m.FirstOnlineAt = t0.Add(-30 * time.Second) })
	mutate(&st, name, func(m *MachineState) { m.StallSamples = 2 })
	if acts, _ := Decide(params(), st, o); countKind(acts, ActSSHAct) != 0 {
		t.Fatal("上线不足 60s 不判假活")
	}
	// QAge 1m（< 2m）
	o2, st2 := aFixture()
	o2.Jobs = []observe.Job{queuedJob(1, time.Minute)}
	mutate(&st2, name, func(m *MachineState) { m.StallSamples = 2 })
	if acts, _ := Decide(params(), st2, o2); countKind(acts, ActSSHAct) != 0 {
		t.Fatal("QAge 不足不判假活")
	}
}

// 无配额时只 SSHAct（N=max，不建替换机）。
func TestStallNoQuotaOnlyRestart(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	p := params()
	p.MaxRunners = 2
	o.Machines = []machine.Machine{droplet(name, time.Hour), droplet("ci-nms-team2-ci-sgp1-02", time.Hour)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	mutate(&st, name, func(m *MachineState) { m.StallSamples = 2 })
	acts, _ := Decide(p, st, o)
	if countKind(acts, ActCreate) != 0 || countKind(acts, ActSSHAct) != 1 {
		t.Fatalf("无配额应只重启：%v", acts)
	}
}

// 同仓 24h 内第 2 次假活 → 事故 A。
func TestStallSecondTimeEscalates(t *testing.T) {
	o, st := aFixture()
	name := "ci-nms-team2-ci-sgp1-01"
	mutate(&st, name, func(m *MachineState) { m.StallSamples = 2 })
	st.StallEvents = []time.Time{t0.Add(-3 * time.Hour)} // 24h 内已有一次
	acts, _ := Decide(params(), st, o)
	if !hasIncident(acts, "A") {
		t.Fatalf("24h 内第 2 次假活应开事故：%v", acts)
	}
}

// ---------- 场景 B 冷启失败与熔断（§8.1）----------

func bFixture(age time.Duration) (observe.Obs, RepoState) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Jobs = []observe.Job{queuedJob(1, age)}
	o.Machines = []machine.Machine{droplet(name, age)}
	return o, RepoState{}
}

// 7m59s 不判、8m01s 判。
func TestColdStartTimeoutBoundary(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		want bool
	}{
		{7*time.Minute + 59*time.Second, false},
		{8*time.Minute + 1*time.Second, true},
	} {
		o, st := bFixture(c.age)
		acts, _ := Decide(params(), st, o)
		if got := countKind(acts, ActForceDelete) == 1; got != c.want {
			t.Fatalf("age=%v want 判死=%v got=%v（%v）", c.age, c.want, got, acts)
		}
	}
}

// 判定后：替换 + 删旧 + Fails++。
func TestColdStartFailReplacesAndCounts(t *testing.T) {
	o, st := bFixture(9 * time.Minute)
	acts, st2 := Decide(params(), st, o)
	if countKind(acts, ActCreate) != 1 || countKind(acts, ActForceDelete) != 1 {
		t.Fatalf("B 应先建后删：%v", acts)
	}
	if st2.Breaker.Fails != 1 {
		t.Fatalf("Fails 应计 1，got %d", st2.Breaker.Fails)
	}
}

// 熔断打开的事故按仓级主体（breaker）只开一个：同轮两台冷启失败、Fails
// 达上限 → 只 1 个 B 事故（2026-11-21 冒烟 S4 实测按机器开了 #4/#5 两个）。
func TestBreakerOpenSingleRepoLevelIncident(t *testing.T) {
	n1, n2 := "ci-nms-team2-ci-sgp1-01", "ci-nms-team2-ci-sgp1-02"
	o := obs()
	o.Jobs = []observe.Job{queuedJob(1, 9*time.Minute)}
	o.Machines = []machine.Machine{droplet(n1, 9*time.Minute), droplet(n2, 9*time.Minute)}
	st := RepoState{Breaker: Breaker{Fails: 2}} // 本轮第 3、4 次
	acts, st2 := Decide(params(), st, o)
	if st2.Breaker.Fails != 4 || st2.Breaker.State != "open" {
		t.Fatalf("Fails 应 4 且 open：%+v", st2.Breaker)
	}
	var incs []Action
	for _, a := range acts {
		if a.Kind == ActIncident && a.IncidentType == "B" {
			incs = append(incs, a)
		}
	}
	if len(incs) != 1 || incs[0].IncidentKey != "breaker" {
		t.Fatalf("同轮双机熔断只应开 1 个仓级事故（key=breaker），got %d 个：%v", len(incs), incs)
	}
	// 已 open 期间又一台超龄（如建机竞速的迟失败者）→ 不再开新事故
	o2 := obs()
	o2.Jobs = []observe.Job{queuedJob(1, 9*time.Minute)}
	o2.Machines = []machine.Machine{droplet("ci-nms-team2-ci-sgp1-03", 9*time.Minute)}
	st3 := RepoState{Breaker: Breaker{State: "open", Fails: 4, OpenedAt: t0.Add(-time.Minute), Cooldown: 30 * time.Minute}}
	acts3, _ := Decide(params(), st3, o2)
	for _, a := range acts3 {
		if a.Kind == ActIncident && a.IncidentType == "B" {
			t.Fatalf("open 期间的后续 B 不应再开事故：%v", a)
		}
	}
}

// 熔断 3 次 open；open 内不建；30m 后 half-open 只建 1 台；试探成功 closed。
func TestBreakerLifecycle(t *testing.T) {
	o, st := bFixture(9 * time.Minute)
	st.Breaker.Fails = 2 // 本轮第 3 次
	acts, st2 := Decide(params(), st, o)
	if st2.Breaker.State != "open" || !hasIncident(acts, "B") {
		t.Fatalf("3 次应 open + 事故：%v %+v", acts, st2.Breaker)
	}
	// open 内（10m）不建
	st3 := RepoState{Breaker: Breaker{State: "open", OpenedAt: t0.Add(-10 * time.Minute), Cooldown: 30 * time.Minute}}
	o3 := obs()
	o3.Jobs = []observe.Job{queuedJob(1, time.Minute)}
	if acts, _ := Decide(params(), st3, o3); countKind(acts, ActCreate) != 0 {
		t.Fatal("open 内不应建机")
	}
	// 31m 后 half-open，允许 1 台试探（Q=7 也只建 1）
	st4 := RepoState{Breaker: Breaker{State: "open", OpenedAt: t0.Add(-31 * time.Minute), Cooldown: 30 * time.Minute}}
	o4 := obs()
	for i := 0; i < 7; i++ {
		o4.Jobs = append(o4.Jobs, queuedJob(int64(i), time.Minute))
	}
	acts4, st5 := Decide(params(), st4, o4)
	if n := countKind(acts4, ActCreate); n != 1 {
		t.Fatalf("half-open 试探只建 1 台，got %d（%v）", n, acts4)
	}
	if st5.Breaker.State != "half-open" {
		t.Fatalf("冷却期满应转 half-open：%+v", st5.Breaker)
	}
	// 试探机 online → closed 且 Fails=0
	name := "ci-nms-team2-ci-sgp1-05"
	o5 := obs()
	o5.Machines = []machine.Machine{droplet(name, 3*time.Minute)}
	o5.Runners = []observe.Runner{onlineRunner(name, false)}
	st6 := RepoState{
		Breaker:  Breaker{State: "half-open", Fails: 3, OpenedAt: t0.Add(-31 * time.Minute), Cooldown: 30 * time.Minute},
		Machines: map[string]MachineState{name: {}},
	}
	_, st7 := Decide(params(), st6, o5)
	if st7.Breaker.State != "closed" || st7.Breaker.Fails != 0 {
		t.Fatalf("试探成功应 closed 且 Fails=0：%+v", st7.Breaker)
	}
}

// 试探失败回 open 且冷却翻倍（30m→1h→2h→4h 封顶）。
func TestBreakerCooldownDoubles(t *testing.T) {
	o, _ := bFixture(9 * time.Minute)
	st := RepoState{Breaker: Breaker{State: "half-open", Fails: 3, OpenedAt: t0.Add(-31 * time.Minute), Cooldown: 30 * time.Minute}}
	_, st2 := Decide(params(), st, o)
	if st2.Breaker.State != "open" || st2.Breaker.Cooldown != time.Hour {
		t.Fatalf("半开试探失败应回 open 且冷却 30m→1h：%+v", st2.Breaker)
	}
	st3 := RepoState{Breaker: Breaker{State: "half-open", Fails: 4, Cooldown: time.Hour}}
	_, st4 := Decide(params(), st3, o)
	if st4.Breaker.Cooldown != 2*time.Hour {
		t.Fatalf("第二次翻倍应 2h：%+v", st4.Breaker)
	}
	st5 := RepoState{Breaker: Breaker{State: "half-open", Fails: 5, Cooldown: 4 * time.Hour}}
	_, st6 := Decide(params(), st5, o)
	if st6.Breaker.Cooldown != 4*time.Hour {
		t.Fatalf("冷却封顶 4h：%+v", st6.Breaker)
	}
}

// ---------- 场景 C 掉线（§8.1）----------

// offline 1 轮不判、2 轮判。
func TestOfflineTwoSamples(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Jobs = []observe.Job{queuedJob(1, time.Minute)}
	o.Machines = []machine.Machine{droplet(name, time.Hour)}
	o.Runners = []observe.Runner{{ID: 1, Name: name, Status: "offline", Busy: false}}
	st := RepoState{Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-time.Hour)}}}
	// 第 1 轮：计数
	acts, st2 := Decide(params(), st, o)
	if countKind(acts, ActForceDelete) != 0 || st2.Machines[name].OfflineSamples != 1 {
		t.Fatalf("第 1 轮只计数：%v %+v", acts, st2.Machines[name])
	}
	// 第 2 轮：判死 + 替换
	acts2, st3 := Decide(params(), st2, o)
	if !hasActionOn(acts2, ActForceDelete, name) || !st3.Machines[name].Bad {
		t.Fatalf("第 2 轮应判死强删：%v", acts2)
	}
	if countKind(acts2, ActCreate) != 1 {
		t.Fatalf("掉线应先建替换：%v", acts2)
	}
	if st3.Breaker.Fails != 0 {
		t.Fatal("C 不计熔断")
	}
}

// 从未 online 的走 B 不走 C。
func TestNeverOnlineIsBNotC(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Machines = []machine.Machine{droplet(name, 9*time.Minute)} // 超冷启窗
	o.Runners = []observe.Runner{{ID: 1, Name: name, Status: "offline"}}
	acts, st2 := Decide(params(), RepoState{}, o)
	if !hasActionOn(acts, ActForceDelete, name) || st2.Breaker.Fails != 1 {
		t.Fatalf("从未 online 超龄应走 B（计熔断）：%v %+v", acts, st2.Breaker)
	}
	if len(st2.OfflineEvents) != 0 {
		t.Fatal("不应记 C 事件")
	}
}

// ---------- 场景 D job 卡死（§8.1）----------

// 59m 不告、61m 告、91m ForceDelete（口径：本机 in_progress job 的 StartedAt，
// 复审修正 3——机器级 BusySince 会误杀连续接单的健康机器）。
func TestJobStuckThresholds(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	mk := func(jobAge time.Duration) (observe.Obs, RepoState) {
		o := obs()
		o.Machines = []machine.Machine{droplet(name, 2*time.Hour)}
		o.Runners = []observe.Runner{onlineRunner(name, true)}
		o.Jobs = []observe.Job{{
			ID: 5, RunID: 1, Status: "in_progress", RunnerName: name,
			CreatedAt: t0.Add(-jobAge), StartedAt: t0.Add(-jobAge),
		}}
		st := RepoState{Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-2 * time.Hour)}}}
		return o, st
	}
	for _, c := range []struct {
		busy      time.Duration
		wantAlert bool
		wantKill  bool
	}{
		{59 * time.Minute, false, false},
		{61 * time.Minute, true, false},
		{91 * time.Minute, true, true},
	} {
		o, st := mk(c.busy)
		acts, _ := Decide(params(), st, o)
		if got := hasIncident(acts, "D"); got != c.wantAlert {
			t.Fatalf("busy=%v 告警 want=%v got=%v", c.busy, c.wantAlert, got)
		}
		if got := hasActionOn(acts, ActForceDelete, name); got != c.wantKill {
			t.Fatalf("busy=%v 击杀 want=%v got=%v（%v）", c.busy, c.wantKill, got, acts)
		}
	}
}

// ---------- 场景 E（§8.1）----------

// 不可匹配 job 不计入 Q、不建机；5m 后出事故动作。
func TestUnmatchedNoCreateButIncident(t *testing.T) {
	o := obs()
	o.UnmatchedQueued = []observe.Job{{ID: 9, RunID: 100, Status: "queued", CreatedAt: t0.Add(-1 * time.Minute), Labels: []string{"nonexistent"}}}
	acts, _ := Decide(params(), RepoState{}, o)
	if countKind(acts, ActCreate) != 0 || hasIncident(acts, "E") {
		t.Fatalf("1m 不建机也不开事故：%v", acts)
	}
	o2 := obs()
	o2.UnmatchedQueued = []observe.Job{{ID: 9, RunID: 100, Status: "queued", CreatedAt: t0.Add(-6 * time.Minute), Labels: []string{"nonexistent"}}}
	acts2, _ := Decide(params(), RepoState{}, o2)
	if !hasIncident(acts2, "E") {
		t.Fatalf("≥5m 应开事故 E：%v", acts2)
	}
}

// ---------- 场景 G（§8.1）----------

// 外部 runner 永不出现在任何删除/ssh 动作里。
func TestExternalRunnerNeverTouched(t *testing.T) {
	o := obs()
	o.Jobs = []observe.Job{queuedJob(1, 5*time.Minute)} // QAge 5m ≥ 2m
	o.Runners = []observe.Runner{
		{ID: 7, Name: "gitrunner-sgp2", Status: "online", Busy: false}, // 外部
	}
	acts, _ := Decide(params(), RepoState{}, o)
	if !hasIncident(acts, "G") {
		t.Fatalf("外部假活应开告知事故：%v", acts)
	}
	for _, a := range acts {
		if (a.Kind == ActDrain || a.Kind == ActForceDelete || a.Kind == ActSSHAct || a.Kind == ActRemoveReg) && a.RunnerName == "gitrunner-sgp2" {
			t.Fatalf("外部 runner 不得被操作：%v", a)
		}
	}
}

// ---------- 场景 H（§8.1 口径）----------

// 连续 10 轮 Complete=false 开事故。
func TestObserveFailStreak(t *testing.T) {
	o := obs()
	o.Complete = false
	st := RepoState{ObserveFailStreak: 9}
	acts, st2 := Decide(params(), st, o)
	if !hasIncident(acts, "H") {
		t.Fatal("第 10 轮观察失败应开事故")
	}
	if st2.ObserveFailStreak != 10 {
		t.Fatalf("streak 应 10，got %d", st2.ObserveFailStreak)
	}
	// 恢复一轮即清零
	o2 := obs()
	_, st3 := Decide(params(), st2, o2)
	if st3.ObserveFailStreak != 0 {
		t.Fatal("观察恢复应清零 streak")
	}
}

// ---------- 场景 I 建机报错回填（§3.6.5 I）----------

func TestRecordCreateResultBackoff(t *testing.T) {
	p := params()
	st := &RepoState{}
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute} {
		_, esc := RecordCreateResult(p, st, t0, errFail{})
		if st.CreateBackoffUntil != t0.Add(want) {
			t.Fatalf("第 %d 次失败退避应 %v，got %v", i+1, want, st.CreateBackoffUntil.Sub(t0))
		}
		if esc != (i == 2) { // 第 3 次升级
			t.Fatalf("第 %d 次升级判定不对", i+1)
		}
	}
	// 成功清零
	RecordCreateResult(p, st, t0, nil)
	if st.CreateFailStreak != 0 || !st.CreateBackoffUntil.IsZero() {
		t.Fatal("成功应清退避")
	}
	// 退避期内 Decide 不建
	o := obs()
	o.Jobs = []observe.Job{queuedJob(1, time.Minute)}
	st2 := RepoState{CreateBackoffUntil: t0.Add(10 * time.Minute)}
	if acts, _ := Decide(params(), st2, o); countKind(acts, ActCreate) != 0 {
		t.Fatal("退避期内不应建机")
	}
}

type errFail struct{}

func (errFail) Error() string { return "boom" }
