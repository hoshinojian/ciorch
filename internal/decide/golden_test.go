// golden 回放夹具（docs/PROVIDER-ABSTRACTION.md §5.1）：把一组有代表性的
// 观察序列跑过 Decide，逐轮序列化 (actions, newState) 存成 golden 文件。
// Provider 抽象重构（PR ③）只允许"改名"级改动——之后同一组输入的输出必须
// 与 golden 完全一致；差异即决策语义变化，直接打回。
//
// 序列化只含输出（Action/RepoState），不含输入 Obs——输入由本文件的构造
// 代码定义，跟随类型改名同步调整；输出里的字段（字符串/时间/整数）不含
// 任何提供商类型，故 golden 在重构前后字节稳定。
//
// 重新生成：go test ./internal/decide -run TestGoldenReplay -update
package decide

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/observe"
)

var updateGolden = flag.Bool("update", false, "重新生成 golden 回放文件")

// goldenRound 是一轮回放的输出快照。
type goldenRound struct {
	Round   int       `json:"round"`
	Actions []Action  `json:"actions"`
	State   RepoState `json:"state"`
}

// goldenScenario 是一个多轮回放：init 状态起，各轮 Obs 依次输入，
// 上一轮输出状态作为下一轮输入（真实守护循环的形态）。
type goldenScenario struct {
	name string
	init RepoState
	obs  []observe.Obs
}

// 场景覆盖（§5.1"有代表性"）：扩容、空闲缩容、A 假活全周期、
// B 冷启失败→熔断→半开→恢复、C 掉线两次升级、D 卡死两档、
// H 观察冻结（含硬寿命例外）、E label 无人满足、G 外部 runner。
func goldenScenarios() []goldenScenario {
	n01, n02, n03, n04 := "ci-nms-team2-ci-sgp1-01", "ci-nms-team2-ci-sgp1-02", "ci-nms-team2-ci-sgp1-03", "ci-nms-team2-ci-sgp1-04"
	q7 := func(o *observe.Obs) {
		for i := 0; i < 7; i++ {
			o.Jobs = append(o.Jobs, queuedJob(int64(i), time.Minute))
		}
	}
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	stallObs := func(now time.Time) observe.Obs { // Q=1（3m）、机器 online 空闲
		o := obs()
		o.Now = now
		o.Jobs = []observe.Job{queuedJob(1, 3*time.Minute)}
		o.Machines = []machine.Machine{droplet(n01, time.Hour)}
		o.Runners = []observe.Runner{onlineRunner(n01, false)}
		return o
	}

	return []goldenScenario{
		{
			name: "scale-up",
			obs: []observe.Obs{
				func() observe.Obs { o := obs(); q7(&o); return o }(), // 2×Create
				func() observe.Obs {
					o := obs()
					o.Now = at(20 * time.Second)
					q7(&o) // 两台冷启在途
					o.Machines = []machine.Machine{droplet(n01, 20*time.Second), droplet(n02, 20*time.Second)}
					return o
				}(),
			},
		},
		{
			name: "idle-teardown",
			init: RepoState{Machines: map[string]MachineState{n01: {
				FirstOnlineAt: at(-30 * time.Minute), IdleSince: at(-19 * time.Minute),
			}}},
			obs: []observe.Obs{
				func() observe.Obs {
					o := obs()
					o.Machines = []machine.Machine{droplet(n01, 30*time.Minute)}
					o.Runners = []observe.Runner{onlineRunner(n01, false)}
					return o
				}(),
				func() observe.Obs {
					o := obs()
					o.Now = at(2 * time.Minute)
					o.Machines = []machine.Machine{droplet(n01, 32*time.Minute)}
					o.Runners = []observe.Runner{onlineRunner(n01, false)}
					return o
				}(), // 空闲 21m → Drain
				func() observe.Obs {
					o := obs()
					o.Now = at(4 * time.Minute)
					o.Machines = []machine.Machine{droplet(n01, 34*time.Minute)}
					return o
				}(), // 注册已消失、机器还在 → Bad 重发 Drain（R7）
			},
		},
		{
			name: "stall-a",
			init: RepoState{Machines: map[string]MachineState{n01: {FirstOnlineAt: at(-5 * time.Minute), IdleSince: at(-3 * time.Minute)}}},
			obs: []observe.Obs{
				stallObs(t0),                    // 样本 1
				stallObs(t0),                    // 样本 2
				stallObs(t0),                    // 样本 3 → Create + SSHAct + Remedy=restarting
				stallObs(at(150 * time.Second)), // 观察窗（2m）过后仍不接活 → Bad + Drain
				func() observe.Obs { // 换 -02：先 online 一轮（FirstOnlineAt 记账，未满 60s 不判）
					o := obs()
					o.Jobs = []observe.Job{queuedJob(2, 3*time.Minute)}
					o.Machines = []machine.Machine{droplet(n02, 20*time.Minute)}
					o.Runners = []observe.Runner{onlineRunner(n02, false)}
					return o
				}(),
				func() observe.Obs { // +150s（上线 ≥60s 且本机空闲 ≥2m）：样本 1
					o := stallObs(at(150 * time.Second))
					o.Jobs = []observe.Job{queuedJob(2, 4*time.Minute)}
					o.Machines = []machine.Machine{droplet(n02, 20*time.Minute)}
					o.Runners = []observe.Runner{onlineRunner(n02, false)}
					return o
				}(),
				func() observe.Obs { // 样本 2
					o := stallObs(at(150 * time.Second))
					o.Jobs = []observe.Job{queuedJob(2, 4*time.Minute)}
					o.Machines = []machine.Machine{droplet(n02, 20*time.Minute)}
					o.Runners = []observe.Runner{onlineRunner(n02, false)}
					return o
				}(),
				func() observe.Obs { // 样本 3 → 命中：24h 内第 2 次 → 事故 A
					o := stallObs(at(150 * time.Second))
					o.Jobs = []observe.Job{queuedJob(2, 4*time.Minute)}
					o.Machines = []machine.Machine{droplet(n02, 21*time.Minute)}
					o.Runners = []observe.Runner{onlineRunner(n02, false)}
					return o
				}(),
			},
		},
		{
			name: "coldstart-b-breaker",
			obs: []observe.Obs{
				func() observe.Obs {
					o := obs()
					o.Jobs = []observe.Job{queuedJob(1, 9*time.Minute)}
					o.Machines = []machine.Machine{droplet(n01, 9*time.Minute)}
					return o
				}(), // B：ForceDelete + Fails=1
				func() observe.Obs {
					o := obs()
					o.Jobs = []observe.Job{queuedJob(1, 9*time.Minute)}
					o.Machines = []machine.Machine{droplet(n02, 9*time.Minute)}
					return o
				}(), // 第 2 台：Fails=2
				func() observe.Obs {
					o := obs()
					o.Jobs = []observe.Job{queuedJob(1, 9*time.Minute)}
					o.Machines = []machine.Machine{droplet(n03, 9*time.Minute)}
					return o
				}(), // 第 3 台：open + 事故 B
				func() observe.Obs { o := obs(); q7(&o); return o }(), // open 内不建
				func() observe.Obs { // 31m 后 → half-open 试探 1 台
					o := obs()
					o.Now = at(31 * time.Minute)
					q7(&o)
					return o
				}(),
				func() observe.Obs { // 试探机 online → closed、Fails=0
					o := obs()
					o.Now = at(34 * time.Minute)
					o.Machines = []machine.Machine{droplet(n04, 3*time.Minute)}
					o.Runners = []observe.Runner{onlineRunner(n04, false)}
					return o
				}(),
			},
		},
		{
			name: "offline-c",
			init: RepoState{Machines: map[string]MachineState{n01: {FirstOnlineAt: at(-1 * time.Hour)}}},
			obs: []observe.Obs{
				func() observe.Obs { // 样本 1
					o := obs()
					o.Jobs = []observe.Job{queuedJob(1, time.Minute)}
					o.Machines = []machine.Machine{droplet(n01, time.Hour)}
					o.Runners = []observe.Runner{{ID: 1, Name: n01, Status: "offline"}}
					return o
				}(),
				func() observe.Obs { // 判死：ForceDelete + Create
					o := obs()
					o.Jobs = []observe.Job{queuedJob(1, 2*time.Minute)}
					o.Machines = []machine.Machine{droplet(n01, time.Hour)}
					o.Runners = []observe.Runner{{ID: 1, Name: n01, Status: "offline"}}
					return o
				}(),
				func() observe.Obs { // 换 -02，先 online 一轮（FirstOnlineAt 记账）
					o := obs()
					o.Jobs = []observe.Job{queuedJob(2, time.Minute)}
					o.Machines = []machine.Machine{droplet(n02, time.Hour)}
					o.Runners = []observe.Runner{onlineRunner(n02, false)}
					return o
				}(),
				func() observe.Obs { // -02 掉线，第 1 个样本
					o := obs()
					o.Jobs = []observe.Job{queuedJob(2, time.Minute)}
					o.Machines = []machine.Machine{droplet(n02, time.Hour)}
					o.Runners = []observe.Runner{{ID: 2, Name: n02, Status: "offline"}}
					return o
				}(),
				func() observe.Obs { // 第 2 个样本 → 判死 + 24h 内第 2 次 → 事故 C
					o := obs()
					o.Jobs = []observe.Job{queuedJob(2, 2*time.Minute)}
					o.Machines = []machine.Machine{droplet(n02, time.Hour)}
					o.Runners = []observe.Runner{{ID: 2, Name: n02, Status: "offline"}}
					return o
				}(),
			},
		},
		{
			name: "job-stuck-d",
			obs: []observe.Obs{
				func() observe.Obs { // 61m → 事故 D
					o := obs()
					o.Machines = []machine.Machine{droplet(n01, 2*time.Hour)}
					o.Runners = []observe.Runner{onlineRunner(n01, true)}
					o.Jobs = []observe.Job{{ID: 5, RunID: 1, Status: "in_progress", RunnerName: n01, CreatedAt: at(-61 * time.Minute), StartedAt: at(-61 * time.Minute)}}
					return o
				}(),
				func() observe.Obs { // 91m → 事故 D + ForceDelete
					o := obs()
					o.Machines = []machine.Machine{droplet(n01, 2*time.Hour)}
					o.Runners = []observe.Runner{onlineRunner(n01, true)}
					o.Jobs = []observe.Job{{ID: 5, RunID: 1, Status: "in_progress", RunnerName: n01, CreatedAt: at(-91 * time.Minute), StartedAt: at(-91 * time.Minute)}}
					return o
				}(),
			},
		},
		{
			name: "frozen-h",
			obs: []observe.Obs{
				func() observe.Obs { // jobs 一路失败：整轮冻结（只累计 streak）
					o := obs()
					o.Complete, o.JobsOK = false, false
					o.Jobs = []observe.Job{queuedJob(1, time.Minute)}
					return o
				}(),
				func() observe.Obs { // droplets 一路成功：硬寿命回收仍执行
					o := obs()
					o.Complete, o.JobsOK, o.MachinesOK = false, false, true
					o.RunnersOK = false
					o.Machines = []machine.Machine{droplet(n01, 9*time.Hour)}
					return o
				}(),
			},
		},
		{
			name: "unmatched-e",
			obs: []observe.Obs{
				func() observe.Obs {
					o := obs()
					o.UnmatchedQueued = []observe.Job{{ID: 9, RunID: 100, Status: "queued", CreatedAt: at(-6 * time.Minute), Labels: []string{"nonexistent"}}}
					return o
				}(),
			},
		},
		{
			name: "external-g",
			obs: []observe.Obs{
				func() observe.Obs {
					o := obs()
					o.Jobs = []observe.Job{queuedJob(1, 5*time.Minute)}
					o.Runners = []observe.Runner{{ID: 7, Name: "gitrunner-sgp2", Status: "online"}}
					return o
				}(),
			},
		},
	}
}

// TestGoldenReplay 逐场景回放并与 golden 文件比对（-update 生成）。
func TestGoldenReplay(t *testing.T) {
	for _, sc := range goldenScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			st := sc.init
			rounds := []goldenRound{}
			for i, o := range sc.obs {
				acts, st2 := Decide(params(), st, o)
				rounds = append(rounds, goldenRound{Round: i, Actions: acts, State: st2})
				st = st2
			}
			got, err := json.MarshalIndent(rounds, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "golden", sc.name+".json")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读 golden 失败（先 go test ./internal/decide -run TestGoldenReplay -update 生成）: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("回放输出与 golden 不一致（决策语义变化或场景构造变化；\n有意变更须显式重新生成并在 PR 里说明差异）：\n--- golden ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}
