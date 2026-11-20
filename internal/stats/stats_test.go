package stats

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

func job(name string, createdOff, startedOff, completedOff time.Duration, runner string) Job {
	j := Job{Name: name, CreatedAt: t0.Add(createdOff), RunnerName: runner}
	if startedOff >= 0 {
		j.StartedAt = t0.Add(startedOff)
	}
	if completedOff >= 0 {
		j.CompletedAt = t0.Add(completedOff)
	}
	return j
}

// 排队等待：started - created；分位数 nearest-rank。
func TestQueueWaitPercentiles(t *testing.T) {
	// 5 个 job 排队 1..5 分钟
	runs := []Run{{CreatedAt: t0, Jobs: []Job{
		job("lint", 0, 1*time.Minute, 2*time.Minute, "r1"),
		job("test", 0, 2*time.Minute, 14*time.Minute, "r1"),
	}}}
	// 凑满 5 个样本（三个 run）
	runs = append(runs,
		Run{CreatedAt: t0.Add(time.Hour), Jobs: []Job{
			job("build", 0, 3*time.Minute, 5*time.Minute, "r1"),
			job("sqlc", 0, 4*time.Minute, 5*time.Minute, "r1"),
		}},
		Run{CreatedAt: t0.Add(2 * time.Hour), Jobs: []Job{
			job("gitleaks", 0, 5*time.Minute, 6*time.Minute, "r1"),
		}})
	rep := Compute("a/b", runs)
	if rep.JobsStarted != 5 {
		t.Fatalf("应 5 个已开跑 job，got %d", rep.JobsStarted)
	}
	if rep.QueueWaitP50 != 3*time.Minute {
		t.Fatalf("p50 应 3m（nearest-rank 第 3 个），got %v", rep.QueueWaitP50)
	}
	if rep.QueueWaitP95 != 5*time.Minute {
		t.Fatalf("p95 应 5m（第 5 个），got %v", rep.QueueWaitP95)
	}
	if rep.QueueWaitMax != 5*time.Minute {
		t.Fatalf("max 应 5m，got %v", rep.QueueWaitMax)
	}
}

// 未开跑的 job（还在排队/被取消）不计入排队等待样本。
func TestUnstartedJobsExcluded(t *testing.T) {
	runs := []Run{{
		CreatedAt: t0,
		Jobs: []Job{
			job("lint", 0, 1*time.Minute, 2*time.Minute, "r1"),
			{Name: "test", CreatedAt: t0}, // 从未开跑（无 StartedAt）
		},
	}}
	rep := Compute("a/b", runs)
	if rep.JobsStarted != 1 || rep.QueueWaitMax != time.Minute {
		t.Fatalf("未开跑 job 应排除: %+v", rep)
	}
}

// 冷启代理：窗口内 runner 首次出现的那个 job，其排队等待含建机时间。
func TestColdStartProxyFirstSeenRunner(t *testing.T) {
	runs := []Run{
		{CreatedAt: t0, Jobs: []Job{
			// r1 首个 job：排队 4m（含冷启）
			job("lint", 0, 4*time.Minute, 5*time.Minute, "r1"),
		}},
		{CreatedAt: t0.Add(time.Hour), Jobs: []Job{
			// r1 第二个 job：排队 30s（热机）
			job("test", 0, 30*time.Second, 12*time.Minute, "r1"),
			// r2 首个 job：排队 3m（含冷启）
			job("build", 0, 3*time.Minute, 4*time.Minute, "r2"),
		}},
	}
	rep := Compute("a/b", runs)
	if rep.ColdStart.Samples != 2 {
		t.Fatalf("应 2 个冷启样本（r1/r2 各一），got %d", rep.ColdStart.Samples)
	}
	wantAvg := (4*time.Minute + 3*time.Minute) / 2
	if rep.ColdStart.Avg != wantAvg {
		t.Fatalf("冷启均值应 %v，got %v", wantAvg, rep.ColdStart.Avg)
	}
	if rep.ColdStart.Max != 4*time.Minute {
		t.Fatalf("冷启 max 应 4m，got %v", rep.ColdStart.Max)
	}
}

// 名字复用不算同一台机器：删机重建后 vpsctl 从 01 重新计数、名字相同但
// runner_id 是新注册。冷启代理必须按 id 区分（2026-11-20 实测：
// cirunner-team2-ci-sgp1-01 一名下 14 个 id）。
func TestColdStartProxyNameReusedDifferentRunnerID(t *testing.T) {
	runs := []Run{
		{CreatedAt: t0, Jobs: []Job{
			// 机器 A（id 23）首 job：排队 4m（含冷启）
			{ID: 1, Name: "lint", CreatedAt: t0, StartedAt: t0.Add(4 * time.Minute), RunnerID: 23, RunnerName: "cirunner-team2-ci-sgp1-01"},
		}},
		{CreatedAt: t0.Add(time.Hour), Jobs: []Job{
			// 机器 B（id 24）与 A 同名：首 job 排队 5m 也应算冷启
			{ID: 2, Name: "lint", CreatedAt: t0.Add(time.Hour), StartedAt: t0.Add(time.Hour + 5*time.Minute), RunnerID: 24, RunnerName: "cirunner-team2-ci-sgp1-01"},
		}},
		{CreatedAt: t0.Add(2 * time.Hour), Jobs: []Job{
			// 机器 B 第二个 job：热机，不算冷启
			{ID: 3, Name: "test", CreatedAt: t0.Add(2 * time.Hour), StartedAt: t0.Add(2*time.Hour + 30*time.Second), RunnerID: 24, RunnerName: "cirunner-team2-ci-sgp1-01"},
		}},
	}
	rep := Compute("a/b", runs)
	if rep.ColdStart.Samples != 2 {
		t.Fatalf("同名不同 id 应算 2 次冷启，got %d", rep.ColdStart.Samples)
	}
	if rep.ColdStart.Max != 5*time.Minute {
		t.Fatalf("冷启 max 应 5m，got %v", rep.ColdStart.Max)
	}
}

// run 总时长：max(job completed) - run.created（用 jobs 口径，不依赖 run.updated_at 语义）。
func TestRunTotalFromJobs(t *testing.T) {
	h := time.Hour
	runs := []Run{
		{CreatedAt: t0, Jobs: []Job{
			job("lint", 0, 1*time.Minute, 2*time.Minute, "r1"),
			job("test", 0, 2*time.Minute, 14*time.Minute, "r1"),
		}},
		{CreatedAt: t0.Add(h), Jobs: []Job{
			job("lint", h, h+30*time.Second, h+90*time.Second, "r1"),
			job("test", h, h+90*time.Second, h+10*time.Minute, "r1"),
		}},
	}
	rep := Compute("a/b", runs)
	if rep.Runs != 2 {
		t.Fatalf("应 2 个 run，got %d", rep.Runs)
	}
	if rep.RunTotalP50 != 10*time.Minute || rep.RunTotalMax != 14*time.Minute {
		t.Fatalf("run 总时长口径不对: p50=%v max=%v", rep.RunTotalP50, rep.RunTotalMax)
	}
}

// 重跑失真排除（审查修正 1）：filter=latest 下未被重跑的 job 保留旧 attempt 的
// started_at、created_at 刷新为新 attempt 时间 → started < created 为负。
// 负值不得进入排队等待/冷启/分 job 样本，单列计数（2026-11-20 真实数据：
// 4 个重跑 run 约 24 个负值 job，曾把 p50 拉低）。
func TestRerunExcludesNegativeWaits(t *testing.T) {
	runs := []Run{{
		ID: 1, RunAttempt: 2, CreatedAt: t0.Add(2 * time.Hour), RunStartedAt: t0.Add(2*time.Hour + time.Minute),
		Jobs: []Job{
			// 重跑的 job：时间对齐，排队 5m，有效
			job("test", 2*time.Hour, 2*time.Hour+5*time.Minute, 2*time.Hour+17*time.Minute, "r1"),
			// 未重跑的 job：started 保留 attempt-1（10:43），created 刷新（12:00）→ 负值
			{ID: 2, Name: "lint", CreatedAt: t0.Add(2 * time.Hour), StartedAt: t0.Add(time.Hour + 43*time.Minute), CompletedAt: t0.Add(time.Hour + 44*time.Minute), RunnerID: 9, RunnerName: "r1"},
		},
	}}
	rep := Compute("a/b", runs)
	if rep.RerunExcludedJobs != 1 {
		t.Fatalf("应排除 1 个负值 job，got %d", rep.RerunExcludedJobs)
	}
	if rep.JobsStarted != 1 {
		t.Fatalf("有效样本应 1，got %d", rep.JobsStarted)
	}
	if rep.QueueWaitP50 != 5*time.Minute || rep.QueueWaitMax != 5*time.Minute {
		t.Fatalf("排队等待应只含有效样本: %+v", rep)
	}
	if rep.ColdStart.Samples != 1 || rep.ColdStart.Avg != 5*time.Minute {
		t.Fatalf("冷启样本不得混入负值: %+v", rep.ColdStart)
	}
	if rep.ByJob["lint"].N != 0 {
		t.Fatalf("分 job 统计不应含排除样本: %+v", rep.ByJob)
	}
}

// 重跑 run 总时长（审查修正 2）：起点用最新 attempt 的 run_started_at，
// 不把两次 attempt 之间的间隔算进去（43586538809：10:42 创建、12:29 才开
// attempt 2，旧口径虚高 1h47m）。
func TestRerunRunTotalFromRunStartedAt(t *testing.T) {
	create := t0                     // 10:42（原始创建）
	at2 := t0.Add(107 * time.Minute) // 12:29（attempt 2 开始）
	runs := []Run{{
		ID: 43586538809, RunAttempt: 2, CreatedAt: create, RunStartedAt: at2,
		Jobs: []Job{
			// 重跑的 job：attempt 2 内 12:30 开跑 13:00 完成
			job("test", 107*time.Minute, 108*time.Minute, 138*time.Minute, "r1"),
			// 未重跑的 job：attempt 1 遗留（11:00 完成，早于 attempt 2 起点，不影响 max）
			{ID: 2, Name: "lint", CreatedAt: at2, StartedAt: t0.Add(43 * time.Minute), CompletedAt: t0.Add(78 * time.Minute), RunnerID: 9, RunnerName: "r1"},
		},
	}}
	rep := Compute("a/b", runs)
	if rep.RerunRuns != 1 {
		t.Fatalf("重跑 run 应计 1，got %d", rep.RerunRuns)
	}
	want := 138*time.Minute - 107*time.Minute // 13:00 - 12:29 = 31m
	if rep.RunTotalMax != want {
		t.Fatalf("run 总时长应按 run_started_at 起点（31m），got %v", rep.RunTotalMax)
	}
	if rep.RerunExcludedJobs != 1 {
		t.Fatalf("attempt-1 遗留 job 应被排除计数，got %d", rep.RerunExcludedJobs)
	}
}

// run_started_at 缺失（零值）时退回 created_at（兼容旧调用方/缺字段）。
func TestRunTotalFallsBackToCreatedAt(t *testing.T) {
	runs := []Run{{ID: 1, CreatedAt: t0, Jobs: []Job{
		job("test", 0, 2*time.Minute, 14*time.Minute, "r1"),
	}}}
	rep := Compute("a/b", runs)
	if rep.RunTotalMax != 14*time.Minute {
		t.Fatalf("无 run_started_at 应退回 created_at，got %v", rep.RunTotalMax)
	}
	if rep.RerunRuns != 0 {
		t.Fatalf("非重跑 run 不应计数，got %d", rep.RerunRuns)
	}
}

// 分 job 名统计：定位哪个 job 是排队大头。
func TestByJobBreakdown(t *testing.T) {
	runs := []Run{{CreatedAt: t0, Jobs: []Job{
		job("test", 0, 5*time.Minute, 17*time.Minute, "r1"),
		job("lint", 0, 30*time.Second, 90*time.Second, "r1"),
	}}}
	rep := Compute("a/b", runs)
	if rep.ByJob["test"].Max != 5*time.Minute || rep.ByJob["lint"].Max != 30*time.Second {
		t.Fatalf("分 job 统计不对: %+v", rep.ByJob)
	}
	if rep.ByJob["test"].N != 1 {
		t.Fatalf("test 应 1 个样本，got %d", rep.ByJob["test"].N)
	}
}
