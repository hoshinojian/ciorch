package budget

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/vpsctlapi"
)

var now = time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)

type fakeLister struct {
	byAccount map[string][]vpsctlapi.Droplet
}

func (f *fakeLister) ListByTag(ctx context.Context, tag, account string) ([]vpsctlapi.Droplet, error) {
	return f.byAccount[account], nil
}

// §8.1：同 token 多账号会同机重复——按 droplet id 去重。
func TestGlobalCountDedupesByID(t *testing.T) {
	l := &fakeLister{byAccount: map[string][]vpsctlapi.Droplet{
		"a1": {
			{ID: "1", Name: "n1"},
			{ID: "2", Name: "n2"},
		},
		"a2": { // 同 token 重复出现
			{ID: "1", Name: "n1"},
			{ID: "3", Name: "n3"},
		},
	}}
	b := &Budget{}
	n, err := b.GlobalCount(context.Background(), l, []string{"a1", "a2"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("去重后应 3 台，got %d", n)
	}
}

// §8.1：当日花费跨机器累计；跨 UTC 日清零。
func TestSpentTodayAccumulatesAndRollover(t *testing.T) {
	b := &Budget{DailyBudgetUSD: 10}
	live := []vpsctlapi.Droplet{
		{ID: "1", CreatedAt: now.Add(-2 * time.Hour), PriceMonthly: 67.2},  // 2h × 0.1 = 0.2
		{ID: "2", CreatedAt: now.Add(-1 * time.Hour), PriceMonthly: 134.4}, // 1h × 0.2 = 0.2
	}
	if got := b.SpentToday(now, live); got < 0.39 || got > 0.41 {
		t.Fatalf("跨机器累计应 0.4，got %f", got)
	}
	// 已删机器入账后删除活机记录，花费不消失
	b.RecordDeletion(now, live[0])
	if got := b.SpentToday(now, live[1:]); got < 0.39 || got > 0.41 {
		t.Fatalf("删机入账后仍应 0.4，got %f", got)
	}
	// 跨 UTC 日清零
	nextDay := now.Add(20 * time.Hour)
	if got := b.SpentToday(nextDay, nil); got != 0 {
		t.Fatalf("跨日应清零，got %f", got)
	}
}

// 配额与预算判定。
func TestCanCreate(t *testing.T) {
	b := &Budget{MaxDroplets: 2, DailyBudgetUSD: 5}
	if ok, _ := b.CanCreate(now, 2, nil, 141); ok {
		t.Fatal("全局配额满应拒绝")
	}
	if ok, _ := b.CanCreate(now, 1, nil, 141); !ok {
		t.Fatal("有余量应允许")
	}
	// 预算：新机 1 小时 141/672 ≈ 0.21 > 0.2 → 拒
	b2 := &Budget{MaxDroplets: 2, DailyBudgetUSD: 0.2}
	if ok, reason := b2.CanCreate(now, 0, nil, 141); ok {
		t.Fatal("预计超日预算应拒绝")
	} else if reason == "" {
		t.Fatal("拒绝应有理由")
	}
}

// §8.1：按 QAge 分配。
func TestAllocateByQAge(t *testing.T) {
	alloc := Allocate(1, 3, []Request{
		{Repo: "a/b", QAge: 30 * time.Second, Want: 2},
		{Repo: "c/d", QAge: 10 * time.Minute, Want: 2},
	})
	// 剩余 2 台：老的 c/d 先拿满 2，a/b 得 0
	if alloc["c/d"] != 2 || alloc["a/b"] != 0 {
		t.Fatalf("按 QAge 分配不对: %+v", alloc)
	}
	alloc2 := Allocate(0, 3, []Request{
		{Repo: "a/b", QAge: time.Hour, Want: 1},
		{Repo: "c/d", QAge: time.Minute, Want: 2},
	})
	if alloc2["a/b"] != 1 || alloc2["c/d"] != 2 {
		t.Fatalf("够分时各自满足: %+v", alloc2)
	}
}

// 快照/恢复：守护重启当日累计不清零；跨日不恢复。
func TestBudgetSnapshotRestore(t *testing.T) {
	b := &Budget{DailyBudgetUSD: 10}
	b.RecordDeletion(now, vpsctlapi.Droplet{PriceMonthly: 67.2, CreatedAt: now.Add(-time.Hour)})
	snap := b.Snapshot(now)
	if snap.SpentToday < 0.09 || snap.SpentToday > 0.11 {
		t.Fatalf("快照应含当日累计: %+v", snap)
	}
	b2 := &Budget{DailyBudgetUSD: 10}
	b2.Restore(now, snap) // 同日恢复
	if got := b2.SpentToday(now, nil); got != snap.SpentToday {
		t.Fatalf("同日应恢复: %f vs %f", got, snap.SpentToday)
	}
	b3 := &Budget{DailyBudgetUSD: 10}
	b3.Restore(now.Add(25*time.Hour), snap) // 跨日
	if got := b3.SpentToday(now.Add(25*time.Hour), nil); got != 0 {
		t.Fatal("跨日不应恢复（自然清零）")
	}
}

// RecordDeletion 只算当日部分（机器跨 UTC 午夜：昨天的小时不算进今天）。
func TestRecordDeletionClampsToDay(t *testing.T) {
	b := &Budget{DailyBudgetUSD: 10}
	// 机器昨天 22:00 创建、今天 06:00 删除 → 只入账今天 6 小时
	created := now.Add(-8 * time.Hour)
	b.RecordDeletion(now, vpsctlapi.Droplet{PriceMonthly: 67.2, CreatedAt: created})
	// 6h × 0.1 = 0.6（若按 8h 会是 0.8）
	if b.SpentToday(now, nil) < 0.59 || b.SpentToday(now, nil) > 0.61 {
		t.Fatalf("应只入账当日部分（6h），got %f", b.SpentToday(now, nil))
	}
}

// Gate 占坑 TTL：Commit 后新机未出现在列表前，名额仍被占；
// 出现在列表后释放；TTL 过期兜底释放。
func TestGateCommitHoldsUntilVisible(t *testing.T) {
	cl := &gateCluster{droplets: map[string]vpsctlapi.Droplet{}}
	b := &Budget{MaxDroplets: 1, DailyBudgetUSD: 100}
	g := NewGate(b, cl, []string{"team2-ci"})
	fakeNow := now
	g.Now = func() time.Time { return fakeNow }

	// 占最后一个名额并建机成功
	r1, ok, _ := g.Reserve(context.Background(), 141)
	if !ok {
		t.Fatal("应有名额")
	}
	g.Commit(r1, "ci-nms-team2-ci-sgp1-01")

	// 新机还没出现在列表：第二个 Reserve 应被拒（名额仍被占）
	if _, ok2, _ := g.Reserve(context.Background(), 141); ok2 {
		t.Fatal("列表未见新机前名额应仍被占")
	}
	// 新机出现在列表 → 占坑释放（名额由 list 计）
	cl.mu.Lock()
	cl.droplets["x"] = vpsctlapi.Droplet{ID: "x", Name: "ci-nms-team2-ci-sgp1-01", PriceMonthly: 141}
	cl.mu.Unlock()
	if _, ok3, reason := g.Reserve(context.Background(), 141); ok3 {
		t.Fatalf("max=1 已被在册机器占用，应拒绝：%s", reason)
	}
	// TTL 兜底：占坑未命名且超时 → 释放
	fakeNow = now.Add(3 * time.Minute)
	cl2 := &gateCluster{droplets: map[string]vpsctlapi.Droplet{}}
	g2 := NewGate(&Budget{MaxDroplets: 1, DailyBudgetUSD: 100}, cl2, []string{"team2-ci"})
	g2.Now = func() time.Time { return now }
	r3, ok4, _ := g2.Reserve(context.Background(), 141)
	if !ok4 {
		t.Fatal("应可占坑")
	}
	_ = r3
	g2.Now = func() time.Time { return fakeNow } // 3 分钟后
	if _, ok5, _ := g2.Reserve(context.Background(), 141); !ok5 {
		t.Fatal("TTL 过期后占坑应释放")
	}
}

type gateCluster struct {
	mu       sync.Mutex
	droplets map[string]vpsctlapi.Droplet
}

func (c *gateCluster) ListByTag(ctx context.Context, tag, account string) ([]vpsctlapi.Droplet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []vpsctlapi.Droplet
	for _, d := range c.droplets {
		out = append(out, d)
	}
	return out, nil
}
