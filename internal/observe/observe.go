// Package observe 把 GitHub + vpsctl 的原始数据拼成 decide.Obs（§3.5）。
// 判据口径：job 粒度而非 run 粒度（P141——run 级 status 滞后，不可靠）；
// label 匹配决定"哪些排队 job 算本执行面的需求"；机器侧信息不参与（§2.2 原则 1）。
package observe

import (
	"context"
	"strings"
	"time"

	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/githubapp"
)

// Job 是一个未完成 job 的观察快照（decide 的需求信号）。
type Job struct {
	ID         int64
	RunID      int64
	Status     string // queued | in_progress
	Labels     []string
	CreatedAt  time.Time // 本 attempt 的创建时间，作排队起点
	StartedAt  time.Time
	RunnerName string
}

// Runner 是 GitHub 侧 runner 注册（本仓全部，含外部——decide 判归属）。
type Runner struct {
	ID     int64
	Name   string
	Status string // online | offline
	Busy   bool
	Labels []string
}

// Droplet 是本仓 tag 下的 droplet。
type Droplet struct {
	ID           string
	Name         string
	CreatedAt    time.Time
	PrivateIP    string
	PublicIP     string
	PriceMonthly float64
}

// Obs 是一轮观察（§3.5 结构）。
type Obs struct {
	Now      time.Time
	Complete bool // 三路观察（runs+jobs / runners / droplets）全部成功
	// 分路成败（观察冻结判据：哪一路失败冻结哪一路的行为，REDESIGN §2.2.7）
	JobsOK          bool
	RunnersOK       bool
	DropletsOK      bool
	Jobs            []Job
	UnmatchedQueued []Job // labels 无法被 profile 满足的 queued job（场景 E）
	Runners         []Runner
	Droplets        []Droplet
	// PendingJobs 是 status ∉ {queued, in_progress} 的未完 job 计数
	// （waiting/requested/pending——不算需求，仅状态展示，§3.5 步骤 3）。
	PendingJobs int
}

// GHSource 是 observe 需要的 GitHub 侧能力（githubapp.Client 实现，测试可 fake）。
type GHSource interface {
	ListRuns(ctx context.Context, repo string, since time.Time) ([]githubapp.Run, error)
	ListJobs(ctx context.Context, repo string, runID int64) ([]githubapp.Job, error)
	ListRunners(ctx context.Context, repo string) ([]githubapp.Runner, error)
}

// VPSSource 是 observe 需要的 DO 侧能力（vpsctlapi 适配器在阶段 2③ 落地）。
type VPSSource interface {
	ListDroplets(ctx context.Context, tag, account string) ([]Droplet, error)
}

// JobCacheEntry 是 webhook 送来的 job 快照（§3.4.5：只用于补齐 run 列表
// 尚未反映的刚出现 job；条目 2 小时过期，同 job id 以 updated 更新者为准）。
type JobCacheEntry struct {
	Job       Job
	UpdatedAt time.Time
}

// Collector 采集一个仓的一轮观察。
type Collector struct {
	GH    GHSource
	VPS   VPSSource
	Cache *JobCache // webhook job 缓存（可为 nil）
	Now   func() time.Time
}

// JobCache 是按 job id 的 webhook 快照缓存。
type JobCache struct {
	entries map[int64]JobCacheEntry
}

// NewJobCache 构造空缓存。
func NewJobCache() *JobCache { return &JobCache{entries: map[int64]JobCacheEntry{}} }

// Put 写入/更新一条快照。
func (c *JobCache) Put(e JobCacheEntry) { c.entries[e.Job.ID] = e }

// Snapshot 返回未过期条目（2 小时，§3.4.5）。
func (c *JobCache) Snapshot(now time.Time) []JobCacheEntry {
	var out []JobCacheEntry
	for _, e := range c.entries {
		if now.Sub(e.UpdatedAt) <= 2*time.Hour {
			out = append(out, e)
		}
	}
	return out
}

// autoLabels 是 runner 注册自动携带的标签，profile 无需声明（§3.3）。
var autoLabels = map[string]bool{"self-hosted": true, "linux": true, "x64": true}

// Matchable 判定 job 的 labels 是否能被 profile 满足（§3.5 步骤 4）：
// job labels（小写化）⊆ {self-hosted, linux, x64} ∪ profile.labels（小写化）。
func Matchable(jobLabels []string, p config.Profile) bool {
	allowed := map[string]bool{}
	for l := range autoLabels {
		allowed[l] = true
	}
	for _, l := range p.Labels {
		allowed[strings.ToLower(l)] = true
	}
	for _, l := range jobLabels {
		if !allowed[strings.ToLower(l)] {
			return false
		}
	}
	return true
}

// Collect 采集一个仓：近 24h 未完成 run → 逐 run jobs（filter=latest）→
// 合并 webhook 缓存 → 状态过滤 → label 匹配 → runners → droplets。
// 任一路失败：该路之后的相关字段留空、Complete=false，其余路照采（§3.5
// 步骤 7——decide 对不完整观察只建不删，§2.2 原则 7）；首错随 Obs 返回供日志。
func (c *Collector) Collect(ctx context.Context, r config.RepoEntry, p config.Profile) (Obs, error) {
	repo := r.Repo
	now := time.Now()
	if c.Now != nil {
		now = c.Now()
	}
	obs := Obs{Now: now, Complete: true, JobsOK: true, RunnersOK: true, DropletsOK: true}
	var firstErr error
	fail := func(err error, leg *bool) {
		obs.Complete = false
		*leg = false
		if firstErr == nil {
			firstErr = err
		}
	}

	byID := map[int64]Job{}
	runs, err := c.GH.ListRuns(ctx, repo, now.Add(-24*time.Hour))
	if err != nil {
		fail(err, &obs.JobsOK)
	} else {
		for _, run := range runs {
			if run.Status == "completed" {
				continue
			}
			jobs, err := c.GH.ListJobs(ctx, repo, run.ID)
			if err != nil {
				fail(err, &obs.JobsOK)
				break
			}
			for _, j := range jobs {
				byID[j.ID] = Job{
					ID: j.ID, RunID: j.RunID, Status: j.Status, Labels: j.Labels,
					CreatedAt: j.CreatedAt, StartedAt: j.StartedAt, RunnerName: j.RunnerName,
				}
			}
		}
		// webhook 缓存合并：同 job id 以 updated 更新者为准（§3.5 步骤 2）。
		if c.Cache != nil {
			for _, e := range c.Cache.Snapshot(now) {
				if old, ok := byID[e.Job.ID]; !ok || e.UpdatedAt.After(jobUpdated(old)) {
					byID[e.Job.ID] = e.Job
				}
			}
		}
		for _, j := range byID {
			switch j.Status {
			case "queued", "in_progress":
				if j.Status == "queued" && !Matchable(j.Labels, p) {
					obs.UnmatchedQueued = append(obs.UnmatchedQueued, j)
					continue
				}
				obs.Jobs = append(obs.Jobs, j)
			default:
				// waiting（环境审批）/requested/pending：不算需求（action_required
				// 的 fork PR 也在此，§7），仅计数。
				obs.PendingJobs++
			}
		}
	}

	ghRunners, err := c.GH.ListRunners(ctx, repo)
	if err != nil {
		fail(err, &obs.RunnersOK)
	} else {
		for _, gr := range ghRunners {
			obs.Runners = append(obs.Runners, Runner{
				ID: gr.ID, Name: gr.Name, Status: gr.Status, Busy: gr.Busy, Labels: gr.LabelNames(),
			})
		}
	}

	droplets, err := c.VPS.ListDroplets(ctx, r.Tag(), p.Account)
	if err != nil {
		fail(err, &obs.DropletsOK)
	} else {
		obs.Droplets = droplets
	}
	return obs, firstErr
}

// jobUpdated 近似取 job 的最后信号时间（started_at 或 created_at）。
func jobUpdated(j Job) time.Time {
	if j.StartedAt.After(j.CreatedAt) {
		return j.StartedAt
	}
	return j.CreatedAt
}
