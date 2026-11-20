// Package stats 是 ciorch stats 的纯计算核心：输入 GitHub 侧 runs/jobs 事实，
// 输出排队等待、冷启代理、run 总时长的统计报告。无 IO、不读时钟。
//
// 口径（docs/REDESIGN.md §9 阶段 1）：
//   - 排队等待 = job started_at - created_at（仅计入已开跑且非重跑失真的 job）
//   - 冷启代理 = 窗口内 runner 注册 id 首次出现的那个 job 的排队等待（含建机时间上界）
//   - run 总时长 = max(job completed_at) - run_started_at（最新 attempt 起点；
//     缺 run_started_at 时退回 created_at）
//   - 重跑失真（filter=latest 的已知行为）：未被重跑的 job 保留第一次 attempt 的
//     started_at，created_at 却刷新为新 attempt 时间 → started < created 为负，
//     计入 RerunExcludedJobs，不进任何样本
//   - 分位数 = nearest-rank（ceil(p/100·N)，确定性、无插值歧义）
package stats

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Run 是一个 workflow run 及其全部 job（GitHub 侧事实，filter=latest 口径）。
type Run struct {
	ID           int64
	RunAttempt   int // 1 = 原始；>1 = 重跑（rerun-failed-jobs 后的最新 attempt）
	CreatedAt    time.Time
	RunStartedAt time.Time // 最新 attempt 的开始时间；零值 = 未知（退回 CreatedAt）
	Jobs         []Job
}

// Job 是 jobs 接口的一个 job 快照。
type Job struct {
	ID          int64
	Name        string
	CreatedAt   time.Time
	StartedAt   time.Time // 零值 = 从未开跑
	CompletedAt time.Time
	RunnerID    int64 // GitHub 注册 id：删机重建后名字可能复用（-01 反复从 01 起），id 必新
	RunnerName  string
}

// JobStat 是按 job 名分组的统计。
type JobStat struct {
	N   int
	P50 time.Duration
	P95 time.Duration
	Max time.Duration
}

// ColdStart 是冷启代理指标（新机器首 job 等待）。
type ColdStart struct {
	Samples int
	Avg     time.Duration
	Max     time.Duration
}

// Report 是一个仓一个窗口的统计报告。
type Report struct {
	Repo        string
	Runs        int
	RerunRuns   int // run_attempt > 1 的 run 数
	JobsStarted int // 有效排队样本数（不含重跑排除）
	// RerunExcludedJobs 是因重跑 started_at<created_at 而排除的 job 数
	// （filter=latest 下未被重跑的 job 保留旧 attempt 的 started_at）。
	RerunExcludedJobs int
	QueueWaitP50      time.Duration
	QueueWaitP95      time.Duration
	QueueWaitMax      time.Duration
	RunTotalP50       time.Duration
	RunTotalP95       time.Duration
	RunTotalMax       time.Duration
	ColdStart         ColdStart
	ByJob             map[string]JobStat
}

// Compute 计算统计报告。runs 顺序任意（内部按时间排序后统计）。
func Compute(repo string, runs []Run) Report {
	rep := Report{Repo: repo, ByJob: map[string]JobStat{}}

	// 按创建时间排序（冷启首见判定需要时序）。
	sorted := append([]Run(nil), runs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })

	var queueWaits, runTotals []time.Duration
	var coldWaits []time.Duration
	seenRunner := map[string]bool{}
	perJob := map[string][]time.Duration{}

	for _, r := range sorted {
		if len(r.Jobs) == 0 {
			continue
		}
		rep.Runs++
		if r.RunAttempt > 1 {
			rep.RerunRuns++
		}
		var lastDone time.Time
		for _, j := range r.Jobs {
			if !j.StartedAt.IsZero() {
				if j.StartedAt.Before(j.CreatedAt) {
					// 重跑失真：旧 attempt 的 started_at + 新 attempt 的 created_at。
					rep.RerunExcludedJobs++
				} else {
					w := j.StartedAt.Sub(j.CreatedAt)
					queueWaits = append(queueWaits, w)
					perJob[j.Name] = append(perJob[j.Name], w)
					rep.JobsStarted++
					// 冷启代理按 runner 注册身份判"新机器"：优先 runner_id（删机
					// 重建后名字会复用，id 不会）；id 缺失时退回名字（不重数即漏数）。
					key := j.RunnerName
					if j.RunnerID != 0 {
						key = fmt.Sprintf("id:%d", j.RunnerID)
					}
					if key != "" && !seenRunner[key] {
						seenRunner[key] = true
						coldWaits = append(coldWaits, w)
					}
				}
			}
			if j.CompletedAt.After(lastDone) {
				lastDone = j.CompletedAt
			}
		}
		if !lastDone.IsZero() {
			start := r.RunStartedAt
			if start.IsZero() {
				start = r.CreatedAt
			}
			if d := lastDone.Sub(start); d > 0 {
				runTotals = append(runTotals, d)
			}
		}
	}

	rep.QueueWaitP50, rep.QueueWaitP95, rep.QueueWaitMax = distill(queueWaits)
	rep.RunTotalP50, rep.RunTotalP95, rep.RunTotalMax = distill(runTotals)
	rep.ColdStart.Samples = len(coldWaits)
	rep.ColdStart.Avg, rep.ColdStart.Max = avgMax(coldWaits)
	for name, ws := range perJob {
		p50, p95, mx := distill(ws)
		rep.ByJob[name] = JobStat{N: len(ws), P50: p50, P95: p95, Max: mx}
	}
	return rep
}

// distill 返回 nearest-rank 分位数与最大值。
func distill(ws []time.Duration) (p50, p95, max time.Duration) {
	if len(ws) == 0 {
		return 0, 0, 0
	}
	sorted := append([]time.Duration(nil), ws...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := func(p float64) time.Duration { return sorted[rankIdx(len(sorted), p)] }
	return rank(50), rank(95), sorted[len(sorted)-1]
}

func rankIdx(n int, p float64) int {
	i := int(math.Ceil(float64(n) * p / 100.0))
	if i < 1 {
		i = 1
	}
	return i - 1
}

func avgMax(ws []time.Duration) (avg, max time.Duration) {
	if len(ws) == 0 {
		return 0, 0
	}
	var sum time.Duration
	for _, w := range ws {
		sum += w
		if w > max {
			max = w
		}
	}
	return sum / time.Duration(len(ws)), max
}
