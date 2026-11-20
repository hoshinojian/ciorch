package decide

import (
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/observe"
)

// 复审修正 3 / 复现 R3：机器连续接单（每轮观察都忙），当前 job 刚开始 →
// 不得判 D；只有当前 job 本身超龄才判。
func TestJobStuckByJobStartedAtNotMachineBusy(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	// 机器已"忙"了 7 小时（连续接单），但当前 job 0 秒前才开始
	o := obs()
	o.Droplets = []observe.Droplet{droplet(name, 7*time.Hour)}
	o.Runners = []observe.Runner{onlineRunner(name, true)}
	o.Jobs = []observe.Job{{
		ID: 9, RunID: 7, Status: "in_progress", RunnerName: name,
		CreatedAt: t0, StartedAt: t0,
	}}
	st := RepoState{Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-7 * time.Hour), BusySince: t0.Add(-7 * time.Hour)}}}
	acts, _ := Decide(params(), st, o)
	if hasIncident(acts, "D") || hasActionOn(acts, ActForceDelete, name) {
		t.Fatalf("当前 job 刚开始不判 D（连续接单是健康机器）：%v", acts)
	}
	// runner busy=true 但 jobs 里查不到对应 in_progress job → 不判 D（不退回机器级）
	o2 := obs()
	o2.Droplets = o.Droplets
	o2.Runners = o.Runners
	o2.Jobs = nil
	st2 := RepoState{Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-7 * time.Hour), BusySince: t0.Add(-7 * time.Hour)}}}
	acts2, _ := Decide(params(), st2, o2)
	if hasIncident(acts2, "D") {
		t.Fatalf("无 job 佐证不判 D：%v", acts2)
	}
}

// 复审修正 4 / 复现 R5：B 判死后 droplet 仍在（DO 删除延迟）——只重发删除，
// 不再建替换机、熔断不重复计数、事件不重复记。
func TestBadMachineIdempotent(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Droplets = []observe.Droplet{droplet(name, 9*time.Minute)}
	st := RepoState{Machines: map[string]MachineState{name: {
		Bad: true, Remedy: "replacing", RemedyAt: t0.Add(-time.Minute),
	}}}
	st.Breaker.Fails = 1
	st.OfflineEvents = []time.Time{t0.Add(-1 * time.Hour)}
	acts, st2 := Decide(params(), st, o)
	if countKind(acts, ActCreate) != 0 {
		t.Fatalf("Bad 机器不建替换机：%v", acts)
	}
	if !hasActionOn(acts, ActForceDelete, name) {
		t.Fatalf("应重发强删：%v", acts)
	}
	if st2.Breaker.Fails != 1 {
		t.Fatalf("熔断不重复计数，got %d", st2.Breaker.Fails)
	}
	if len(st2.OfflineEvents) != 1 {
		t.Fatalf("事件不重复记：%v", st2.OfflineEvents)
	}
	// C 路径同样幂等：Bad+draining 的机器只重发 Drain
	st3 := RepoState{Machines: map[string]MachineState{name: {
		Bad: true, Remedy: "draining", RemedyAt: t0.Add(-time.Minute), FirstOnlineAt: t0.Add(-time.Hour),
	}}}
	acts3, _ := Decide(params(), st3, o)
	if !hasActionOn(acts3, ActDrain, name) || countKind(acts3, ActCreate) != 0 {
		t.Fatalf("Bad+draining 应只重发 Drain：%v", acts3)
	}
}

// 复审修正 5 / 复现 R4：half-open、Q=5、max=3，多轮只建 1 台试探机。
func TestHalfOpenSingleProbeAcrossRounds(t *testing.T) {
	probe := "ci-nms-team2-ci-sgp1-05"
	// 第 1 轮：half-open 无在途 → 建 1 台
	o1 := obs()
	for i := 0; i < 5; i++ {
		o1.Jobs = append(o1.Jobs, queuedJob(int64(i), time.Minute))
	}
	st := RepoState{Breaker: Breaker{State: "open", OpenedAt: t0.Add(-31 * time.Minute), Cooldown: 30 * time.Minute}}
	acts1, st1 := Decide(params(), st, o1)
	if countKind(acts1, ActCreate) != 1 {
		t.Fatalf("第 1 轮应建 1 台试探：%v", acts1)
	}
	// 第 2 轮：试探机 droplet 已出现（冷启在途）→ 不再建
	o2 := obs()
	for i := 0; i < 5; i++ {
		o2.Jobs = append(o2.Jobs, queuedJob(int64(i), 2*time.Minute))
	}
	o2.Droplets = []observe.Droplet{droplet(probe, time.Minute)}
	acts2, st2 := Decide(params(), st1, o2)
	if countKind(acts2, ActCreate) != 0 {
		t.Fatalf("半开已有试探在途不应再建（复现 R4）：%v", acts2)
	}
	if st2.Breaker.State != "half-open" {
		t.Fatalf("应保持 half-open：%+v", st2.Breaker)
	}
	// 第 3 轮同第 2 轮
	o3 := o2
	o3.Jobs = append(o3.Jobs, queuedJob(9, 3*time.Minute))
	acts3, _ := Decide(params(), st2, o3)
	if countKind(acts3, ActCreate) != 0 {
		t.Fatalf("第 3 轮仍不应再建：%v", acts3)
	}
}

// 复审修正 6 / 复现 R6：Drain 失败后机器还在且未 Bad → 清 Remedy 正常重判，
// 空闲超时应再次回收（不卡死到 8h 硬寿命）。
func TestDrainRetryAfterFailure(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Droplets = []observe.Droplet{droplet(name, time.Hour)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	st := RepoState{Machines: map[string]MachineState{name: {
		FirstOnlineAt: t0.Add(-time.Hour), IdleSince: t0.Add(-70 * time.Minute),
		Remedy: "draining", RemedyAt: t0.Add(-50 * time.Minute), // 上次 Drain 失败遗留
	}}}
	acts, st2 := Decide(params(), st, o)
	if !hasActionOn(acts, ActDrain, name) {
		t.Fatalf("清 Remedy 后空闲超时应再次回收：%v", acts)
	}
	if st2.Machines[name].Remedy != "draining" {
		t.Fatalf("新一轮 Drain 应重新记录：%+v", st2.Machines[name])
	}
}

// 复审修正 10：Q=0 时 B/C 判死不建替换机（建了也会被空闲回收，白花钱）。
func TestNoReplacementWhenNoDemand(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	// B 超龄、Q=0
	o := obs()
	o.Droplets = []observe.Droplet{droplet(name, 9*time.Minute)}
	acts, _ := Decide(params(), RepoState{}, o)
	if countKind(acts, ActCreate) != 0 || !hasActionOn(acts, ActForceDelete, name) {
		t.Fatalf("Q=0 的 B 应只删不建：%v", acts)
	}
	// C 掉线、Q=0
	o2 := obs()
	o2.Droplets = o.Droplets
	o2.Runners = []observe.Runner{{ID: 1, Name: name, Status: "offline"}}
	st := RepoState{Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-time.Hour), OfflineSamples: 1}}}
	acts2, _ := Decide(params(), st, o2)
	if countKind(acts2, ActCreate) != 0 {
		t.Fatalf("Q=0 的 C 应只删不建：%v", acts2)
	}
}

// 复审修正 9：命名空间内 offline 注册、无对应 droplet → ActRemoveReg；
// 有对应 droplet（场景 C 领地）与外部 runner 不清。
func TestRemoveStaleRegistration(t *testing.T) {
	o := obs()
	o.Runners = []observe.Runner{
		{ID: 11, Name: "ci-nms-team2-ci-sgp1-07", Status: "offline"},   // 残留：无 droplet
		{ID: 12, Name: "ci-nms-team2-ci-sgp1-01", Status: "offline"},   // 机器在：归 C
		{ID: 13, Name: "gitrunner-sgp2", Status: "offline"},            // 外部：不碰
		{ID: 14, Name: "ci-nms-team2-ci-sgp1-08-a", Status: "offline"}, // rpd=2 残留
	}
	o.Droplets = []observe.Droplet{droplet("ci-nms-team2-ci-sgp1-01", time.Hour)}
	acts, _ := Decide(params(), RepoState{}, o)
	if !hasRemoveReg(acts, "ci-nms-team2-ci-sgp1-07") || !hasRemoveReg(acts, "ci-nms-team2-ci-sgp1-08-a") {
		t.Fatalf("应清无 droplet 的残留注册：%v", acts)
	}
	if hasRemoveReg(acts, "ci-nms-team2-ci-sgp1-01") {
		t.Fatal("机器还在的 offline 注册归 C 管，不在此清")
	}
	if hasRemoveReg(acts, "gitrunner-sgp2") {
		t.Fatal("外部 runner 不碰")
	}
}

func hasRemoveReg(acts []Action, name string) bool {
	for _, a := range acts {
		if a.Kind == ActRemoveReg && a.RunnerName == name {
			return true
		}
	}
	return false
}

// 顺手修：LastActivity 按不同 RunID 记——同一 run 的多个 job 只算一次活动。
func TestLastActivityByRunID(t *testing.T) {
	o := obs()
	// 一个 run 的 7 个 job 都在跑（同 RunID=100）
	for i := 0; i < 7; i++ {
		o.Jobs = append(o.Jobs, observe.Job{
			ID: int64(i), RunID: 100, Status: "in_progress",
			CreatedAt: t0.Add(-10 * time.Minute), StartedAt: t0.Add(-10 * time.Minute),
		})
	}
	st := RepoState{}
	_, st2 := Decide(params(), st, o)
	if len(st2.LastActivity) != 1 {
		t.Fatalf("同 run 多 job 只记一次活动点，got %d 个：%v", len(st2.LastActivity), st2.LastActivity)
	}
	// 两个不同 run → 两个活动点
	o.Jobs = append(o.Jobs, observe.Job{ID: 99, RunID: 200, Status: "in_progress", CreatedAt: t0.Add(-5 * time.Minute), StartedAt: t0.Add(-5 * time.Minute)})
	_, st3 := Decide(params(), st, o)
	if len(st3.LastActivity) != 2 {
		t.Fatalf("两个 run 应两个活动点，got %d", len(st3.LastActivity))
	}
}
