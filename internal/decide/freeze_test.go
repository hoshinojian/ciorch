package decide

import (
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/observe"
)

// 审查修正 2 / 复现 R1：健康忙碌机器，runners 一路失败（Complete=false、
// Runners 为空）——连续多轮也不得 ForceDelete/建替换/开 C 事故。
func TestFreezeRunnersLegFailureNoKill(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	st := RepoState{Machines: map[string]MachineState{name: {
		FirstOnlineAt: t0.Add(-2 * time.Hour), BusySince: t0.Add(-30 * time.Minute),
	}}}
	for round := 0; round < 4; round++ {
		o := obs()
		o.Complete = false
		o.RunnersOK = false
		o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "in_progress", CreatedAt: t0.Add(-30 * time.Minute), StartedAt: t0.Add(-30 * time.Minute), RunnerName: name}}
		o.Droplets = []observe.Droplet{droplet(name, 2*time.Hour)}
		// Runners 为空（一路失败）
		var acts []Action
		acts, st = Decide(params(), st, o)
		for _, a := range acts {
			if a.Kind == ActForceDelete || a.Kind == ActCreate || a.Kind == ActDrain || a.Kind == ActSSHAct {
				t.Fatalf("第 %d 轮冻结期不得动手：%v", round+1, a)
			}
			if a.Kind == ActIncident && a.IncidentType == "C" {
				t.Fatalf("第 %d 轮不得开 C 事故", round+1)
			}
		}
		// 机器记忆不得被清（droplets 一路成功，syncMachines 保留）
		if _, ok := st.Machines[name]; !ok {
			t.Fatalf("第 %d 轮机器记忆被清", round+1)
		}
		// OfflineSamples 不推进（否则恢复后第一轮就凑满两轮误判 C）
		if st.Machines[name].OfflineSamples != 0 {
			t.Fatalf("冻结轮不应推进 OfflineSamples：got %d", st.Machines[name].OfflineSamples)
		}
	}
	// 恢复完整观察：健康忙碌机器无任何动作
	o := obs()
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "in_progress", CreatedAt: t0.Add(-30 * time.Minute), StartedAt: t0.Add(-30 * time.Minute), RunnerName: name}}
	o.Droplets = []observe.Droplet{droplet(name, 2*time.Hour)}
	o.Runners = []observe.Runner{onlineRunner(name, true)}
	acts, _ := Decide(params(), st, o)
	if n := len(acts); n > 0 {
		t.Fatalf("恢复后健康机器应无动作：%v", acts)
	}
}

// 审查修正 2 / 复现 R2：droplets 一路失败（Droplets 为空）、Q=2 ——
// 不得建机（N 不详，宁可不建），且 Machines 记忆不被清空。
func TestFreezeDropletsLegFailureNoOvercreate(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	st := RepoState{Machines: map[string]MachineState{name: {
		FirstOnlineAt: t0.Add(-1 * time.Hour), IdleSince: t0.Add(-10 * time.Minute),
	}}}
	o := obs()
	o.Complete = false
	o.DropletsOK = false
	o.Jobs = []observe.Job{queuedJob(1, time.Minute), queuedJob(2, time.Minute)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	acts, st2 := Decide(params(), st, o)
	if countKind(acts, ActCreate) != 0 {
		t.Fatalf("droplets 一路失败不得建机（会超建）：%v", acts)
	}
	if len(st2.Machines) != 1 || !st2.Machines[name].FirstOnlineAt.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("机器记忆不得被清：%+v", st2.Machines)
	}
	// 也不得因"无 droplet"推进任何判定（如清注册）
	if countKind(acts, ActRemoveReg) != 0 {
		t.Fatalf("冻结期不得清注册：%v", acts)
	}
}

// jobs 一路失败：不建不删（Q 不可知），只累计 H。
func TestFreezeJobsLegFailure(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := obs()
	o.Complete = false
	o.JobsOK = false
	o.Droplets = []observe.Droplet{droplet(name, 30*time.Minute)}
	o.Runners = []observe.Runner{onlineRunner(name, false)}
	st := RepoState{ObserveFailStreak: 9, Machines: map[string]MachineState{name: {FirstOnlineAt: t0.Add(-30 * time.Minute)}}}
	acts, st2 := Decide(params(), st, o)
	if !hasIncident(acts, "H") {
		t.Fatalf("第 10 轮观察失败应开 H 事故：%v", acts)
	}
	if countKind(acts, ActCreate) != 0 || countKind(acts, ActDrain) != 0 {
		t.Fatalf("jobs 一路失败不得建/删：%v", acts)
	}
	_ = st2
}
