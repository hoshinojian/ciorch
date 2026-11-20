package budget

import (
	"context"
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
