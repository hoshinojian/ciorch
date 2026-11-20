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
