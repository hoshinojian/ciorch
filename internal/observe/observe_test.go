package observe

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/githubapp"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/machine/machinetest"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

type fakeGH struct {
	runs        []githubapp.Run
	jobs        map[int64][]githubapp.Job // runID → jobs
	runners     []githubapp.Runner
	failRuns    error
	failOneJobs error // 首个 run 的 jobs 调用失败
	failRunner  error
}

func (f *fakeGH) ListRuns(ctx context.Context, repo string, since time.Time) ([]githubapp.Run, error) {
	if f.failRuns != nil {
		return nil, f.failRuns
	}
	return f.runs, nil
}
func (f *fakeGH) ListJobs(ctx context.Context, repo string, runID int64) ([]githubapp.Job, error) {
	if f.failOneJobs != nil && runID == f.runs[0].ID {
		return nil, f.failOneJobs
	}
	return f.jobs[runID], nil
}
func (f *fakeGH) ListRunners(ctx context.Context, repo string) ([]githubapp.Runner, error) {
	if f.failRunner != nil {
		return nil, f.failRunner
	}
	return f.runners, nil
}

// fakePool 包装 machinetest.Fake，补持续失败开关（FailNextList 只失败一次）。
type fakePool struct {
	*machinetest.Fake
	failList error
}

func (f *fakePool) List(ctx context.Context, tag string) ([]machine.Machine, error) {
	if f.failList != nil {
		return nil, f.failList
	}
	return f.Fake.List(ctx, tag)
}

func newFakePool() *fakePool {
	p := &fakePool{Fake: machinetest.NewFake("go-heavy")}
	p.Now = func() time.Time { return now }
	return p
}

func repo1() config.RepoEntry { return config.RepoEntry{Repo: "hoshinojian/nms", Profile: "go-heavy"} }

func prof() config.Profile {
	return config.Profile{Account: "team2-ci", Region: "sgp1", Labels: []string{"ci"}, RunnersPerDroplet: 1}
}

func collector(gh *fakeGH, pool *fakePool, cache *JobCache) *Collector {
	poolOf := func(r config.RepoEntry, p config.Profile) machine.Provider { return pool }
	return &Collector{GH: gh, PoolOf: poolOf, Cache: cache, Now: func() time.Time { return now }}
}

func j(id int64, status string, created time.Time, labels ...string) githubapp.Job {
	return githubapp.Job{ID: id, RunID: 100, Name: "test", Status: status, CreatedAt: created, Labels: labels}
}

// §8.1：label 匹配大小写、self-hosted 自动标签。
func TestMatchable(t *testing.T) {
	p := prof() // labels: ["ci"]
	cases := map[string][]string{
		"精确匹配":              {"self-hosted", "ci"},
		"大小写不敏感":            {"Self-Hosted", "CI"},
		"自动标签无需 profile 声明": {"self-hosted", "linux", "x64"},
	}
	for name, labels := range cases {
		if !Matchable(labels, p) {
			t.Errorf("%s：%v 应可匹配", name, labels)
		}
	}
	if Matchable([]string{"self-hosted", "ubuntu-latest"}, p) {
		t.Error("profile 外 label（ubuntu-latest）不应匹配")
	}
	if Matchable([]string{"self-hosted", "ci-sandbox"}, p) {
		t.Error("相近但不同的 label 不应匹配")
	}
}

// §8.1：P141 场景——run status=queued 但 jobs 全 completed → Q=0。
func TestP141AllJobsCompletedMeansNoDemand(t *testing.T) {
	gh := &fakeGH{
		runs: []githubapp.Run{{ID: 100, Status: "queued", CreatedAt: now.Add(-10 * time.Minute)}},
		jobs: map[int64][]githubapp.Job{
			100: {j(1, "completed", now.Add(-9*time.Minute), "self-hosted", "ci")},
		},
	}
	obs, err := collector(gh, newFakePool(), nil).Collect(context.Background(), repo1(), prof())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Jobs) != 0 {
		t.Fatalf("jobs 全 completed 不算需求: %+v", obs.Jobs)
	}
	if !obs.Complete {
		t.Fatal("三路成功应 Complete=true")
	}
}

// 未完成 job 状态过滤：queued/in_progress 是需求；waiting/requested 仅计数。
func TestStatusFilterCountsPending(t *testing.T) {
	gh := &fakeGH{
		runs: []githubapp.Run{{ID: 100, Status: "in_progress", CreatedAt: now.Add(-30 * time.Minute)}},
		jobs: map[int64][]githubapp.Job{
			100: {
				j(1, "queued", now.Add(-5*time.Minute), "self-hosted", "ci"),
				j(2, "in_progress", now.Add(-20*time.Minute), "self-hosted", "ci"),
				j(3, "waiting", now.Add(-40*time.Minute), "self-hosted", "ci"),
				j(4, "requested", now.Add(-40*time.Minute), "self-hosted", "ci"),
			},
		},
	}
	obs, err := collector(gh, newFakePool(), nil).Collect(context.Background(), repo1(), prof())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Jobs) != 2 {
		t.Fatalf("queued+in_progress 应 2 个: %+v", obs.Jobs)
	}
	if obs.PendingJobs != 2 {
		t.Fatalf("waiting/requested 应计数 2，got %d", obs.PendingJobs)
	}
}

// label 不满足的 queued job → UnmatchedQueued（场景 E 信号），不计需求。
func TestUnmatchedQueuedSeparated(t *testing.T) {
	gh := &fakeGH{
		runs: []githubapp.Run{{ID: 100, Status: "queued", CreatedAt: now.Add(-6 * time.Minute)}},
		jobs: map[int64][]githubapp.Job{
			100: {
				j(1, "queued", now.Add(-6*time.Minute), "self-hosted", "nonexistent"),
				j(2, "queued", now.Add(-2*time.Minute), "self-hosted", "ci"),
			},
		},
	}
	obs, _ := collector(gh, newFakePool(), nil).Collect(context.Background(), repo1(), prof())
	if len(obs.UnmatchedQueued) != 1 || obs.UnmatchedQueued[0].ID != 1 {
		t.Fatalf("不可满足的 queued job 应入 UnmatchedQueued: %+v", obs.UnmatchedQueued)
	}
	if len(obs.Jobs) != 1 || obs.Jobs[0].ID != 2 {
		t.Fatalf("可满足的才计需求: %+v", obs.Jobs)
	}
}

// completed run 跳过（不拉 jobs）。
func TestCompletedRunsSkipped(t *testing.T) {
	gh := &fakeGH{
		runs: []githubapp.Run{{ID: 100, Status: "completed", CreatedAt: now.Add(-1 * time.Hour)}},
		jobs: map[int64][]githubapp.Job{},
	}
	obs, err := collector(gh, newFakePool(), nil).Collect(context.Background(), repo1(), prof())
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Jobs) != 0 {
		t.Fatalf("completed run 不贡献 job: %+v", obs.Jobs)
	}
}

// webhook 缓存合并：缓存更新者胜过 REST 快照；过期条目丢弃。
func TestWebhookCacheMerge(t *testing.T) {
	restTime := now.Add(-3 * time.Minute)
	gh := &fakeGH{
		runs: []githubapp.Run{{ID: 100, Status: "queued", CreatedAt: restTime}},
		jobs: map[int64][]githubapp.Job{
			100: {j(1, "queued", restTime, "self-hosted", "ci")},
		},
	}
	cache := NewJobCache()
	// job 1：webhook 比REST 新（30s 前收到 queued）——不改变结果但走合并路径
	cache.Put(JobCacheEntry{Job: Job{ID: 1, RunID: 100, Status: "queued", Labels: []string{"self-hosted", "ci"}, CreatedAt: restTime}, UpdatedAt: now.Add(-30 * time.Second)})
	// job 2：只在 webhook 里出现（run 列表尚未反映的刚出现 job）
	cache.Put(JobCacheEntry{Job: Job{ID: 2, RunID: 101, Status: "queued", Labels: []string{"self-hosted", "ci"}, CreatedAt: now.Add(-1 * time.Minute)}, UpdatedAt: now.Add(-1 * time.Minute)})
	// job 3：过期（3 小时前）
	cache.Put(JobCacheEntry{Job: Job{ID: 3, RunID: 102, Status: "queued", Labels: []string{"self-hosted", "ci"}, CreatedAt: now.Add(-3 * time.Hour)}, UpdatedAt: now.Add(-3 * time.Hour)})

	obs, err := collector(gh, newFakePool(), cache).Collect(context.Background(), repo1(), prof())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int64]bool{}
	for _, jb := range obs.Jobs {
		ids[jb.ID] = true
	}
	if !ids[1] || !ids[2] {
		t.Fatalf("REST 与 webhook 的 job 都应在：got %v", ids)
	}
	if ids[3] {
		t.Fatal("过期缓存条目应丢弃")
	}
}

// 任一路失败：Complete=false、部分数据照常返回、其余腿照采。
func TestPartialFailureKeepsOtherLegs(t *testing.T) {
	gh := &fakeGH{
		runs:     []githubapp.Run{{ID: 100, Status: "queued", CreatedAt: now.Add(-5 * time.Minute)}},
		jobs:     map[int64][]githubapp.Job{100: {j(1, "queued", now.Add(-5*time.Minute), "self-hosted", "ci")}},
		runners:  []githubapp.Runner{{ID: 9, Name: "ci-nms-team2-ci-sgp1-01", Status: "online", Busy: false, Labels: []githubapp.RunnerLabel{{ID: 1, Name: "self-hosted"}, {ID: 2, Name: "ci"}}}},
		failRuns: errors.New("TLS handshake timeout"),
	}
	vps := newFakePool()
	vps.Seed(machine.Machine{Pool: "go-heavy", ID: "7", Name: "ci-nms-team2-ci-sgp1-01", CreatedAt: now.Add(-10 * time.Minute), Tags: []string{repo1().Tag()}})
	obs, err := collector(gh, vps, nil).Collect(context.Background(), repo1(), prof())
	if err == nil {
		t.Fatal("失败腿应带回首错")
	}
	if obs.Complete {
		t.Fatal("runs 腿失败应 Complete=false")
	}
	// runners/droplets 腿照采（decide 只建不删仍可用）
	if len(obs.Runners) != 1 || len(obs.Machines) != 1 {
		t.Fatalf("其余腿应照采: runners=%v machines=%v", obs.Runners, obs.Machines)
	}
	if len(obs.Jobs) != 0 {
		t.Fatalf("jobs 腿失败不应有需求信号: %+v", obs.Jobs)
	}
}

// 机器查询按仓 tag 列机（profile→池解析在装配层；账号钉扎是 provider
// 内部行为，见 internal/provider/vpsctl 的 argv 测试）。
func TestMachineQueryUsesRepoTag(t *testing.T) {
	pool := newFakePool()
	if _, err := collector(&fakeGH{}, pool, nil).Collect(context.Background(), repo1(), prof()); err != nil {
		t.Fatal(err)
	}
	if len(pool.ListedTags) == 0 || pool.ListedTags[0] != repo1().Tag() {
		t.Fatalf("应按仓 tag 列机：%v", pool.ListedTags)
	}
}
