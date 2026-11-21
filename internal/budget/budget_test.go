package budget

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/machine/machinetest"
)

var now = time.Date(2026, 11, 20, 6, 0, 0, 0, time.UTC)

const h141 = 141.0 / 672.0 // c2-4vcpu-8gb 的每小时成本（vpsctl 口径）
const h6720 = 67.2 / 672.0 // 0.1 USD/h
const h13440 = 134.4 / 672.0

// §3.4：全局计数对每个池 List(公共标记) 后按 (池, ID) 去重——
// 不同池的机器各计各的（各自账号，物理上不重叠）。
func TestGateCountsByPoolAndID(t *testing.T) {
	a := machinetest.NewFake("pool-a")
	a.Seed(machine.Machine{Pool: "pool-a", ID: "1", Name: "n1", Tags: []string{"ciorch"}})
	a.Seed(machine.Machine{Pool: "pool-a", ID: "2", Name: "n2", Tags: []string{"ciorch"}})
	b := machinetest.NewFake("pool-b")
	b.Seed(machine.Machine{Pool: "pool-b", ID: "1", Name: "n1-again", Tags: []string{"ciorch"}}) // 跨池同 ID：两台
	g := NewGate(&Budget{MaxMachines: 10, DailyBudgetUSD: 100}, map[string]machine.Provider{"pool-a": a, "pool-b": b}, map[string]int{"pool-a": 5, "pool-b": 5}, map[string]float64{"pool-a": 1, "pool-b": 1})
	g.Now = func() time.Time { return now }
	live, err := g.liveMachines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 3 {
		t.Fatalf("(池,ID) 去重后应 3 台，got %d：%+v", len(live), live)
	}
}

// §8.1：当日花费跨机器累计；跨 UTC 日清零。
func TestSpentTodayAccumulatesAndRollover(t *testing.T) {
	b := &Budget{DailyBudgetUSD: 10}
	live := []machine.Machine{
		{ID: "1", CreatedAt: now.Add(-2 * time.Hour), HourlyCost: h6720},  // 2h × 0.1 = 0.2
		{ID: "2", CreatedAt: now.Add(-1 * time.Hour), HourlyCost: h13440}, // 1h × 0.2 = 0.2
	}
	if got := b.SpentToday(now, live); got < 0.39 || got > 0.41 {
		t.Fatalf("跨机器累计应 0.4，got %f", got)
	}
	// 已删机器入账后删除活机记录，花费不消失
	b.RecordDeletion(now, live[0].HourlyCost, live[0].CreatedAt)
	if got := b.SpentToday(now, live[1:]); got < 0.39 || got > 0.41 {
		t.Fatalf("删机入账后仍应 0.4，got %f", got)
	}
	// 跨 UTC 日清零
	nextDay := now.Add(20 * time.Hour)
	if got := b.SpentToday(nextDay, nil); got != 0 {
		t.Fatalf("跨日应清零，got %f", got)
	}
}

// 配额与预算判定（hourly 口径：HourlyCost 由提供商给出，本包不算月价）。
func TestCanCreate(t *testing.T) {
	b := &Budget{MaxMachines: 2, DailyBudgetUSD: 5}
	if ok, _ := b.CanCreate(now, 2, nil, h141); ok {
		t.Fatal("全局配额满应拒绝")
	}
	if ok, _ := b.CanCreate(now, 1, nil, h141); !ok {
		t.Fatal("有余量应允许")
	}
	// 预算：新机 1 小时 ≈ 0.21 > 0.2 → 拒
	b2 := &Budget{MaxMachines: 2, DailyBudgetUSD: 0.2}
	if ok, reason := b2.CanCreate(now, 0, nil, h141); ok {
		t.Fatal("预计超日预算应拒绝")
	} else if reason == "" {
		t.Fatal("拒绝应有理由")
	}
	// HourlyCost=0 的池（自有机器）不消耗预算
	if ok, _ := b2.CanCreate(now, 1, []machine.Machine{{ID: "x", HourlyCost: 0}}, 0); !ok {
		t.Fatal("零成本机器不应触发预算拒绝")
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
	b.RecordDeletion(now, h6720, now.Add(-time.Hour))
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
	b.RecordDeletion(now, h6720, created)
	// 6h × 0.1 = 0.6（若按 8h 会是 0.8）
	if got := b.SpentToday(now, nil); got < 0.59 || got > 0.61 {
		t.Fatalf("应只入账当日部分（6h），got %f", got)
	}
}

// Gate 占坑 TTL：Commit 后新机未出现在列表前，名额仍被占；
// 出现在列表后释放；TTL 过期兜底释放。
func TestGateCommitHoldsUntilVisible(t *testing.T) {
	cl := newGateCluster()
	b := &Budget{MaxMachines: 1, DailyBudgetUSD: 100}
	g := NewGate(b, map[string]machine.Provider{"team2-ci": cl}, map[string]int{"team2-ci": 1}, map[string]float64{"team2-ci": h141})
	fakeNow := now
	g.Now = func() time.Time { return fakeNow }

	// 占最后一个名额并建机成功
	r1, ok, _ := g.Reserve(context.Background(), "team2-ci")
	if !ok {
		t.Fatal("应有名额")
	}
	g.Commit(r1, "ci-nms-team2-ci-sgp1-01")

	// 新机还没出现在列表：第二个 Reserve 应被拒（名额仍被占）
	if _, ok2, _ := g.Reserve(context.Background(), "team2-ci"); ok2 {
		t.Fatal("列表未见新机前名额应仍被占")
	}
	// 新机出现在列表 → 占坑释放（名额由 list 计）
	cl.mu.Lock()
	cl.seed(machine.Machine{Pool: "team2-ci", ID: "x", Name: "ci-nms-team2-ci-sgp1-01", HourlyCost: h141, Tags: []string{"ciorch"}})
	cl.mu.Unlock()
	if _, ok3, reason := g.Reserve(context.Background(), "team2-ci"); ok3 {
		t.Fatalf("max=1 已被在册机器占用，应拒绝：%s", reason)
	}
	// TTL 兜底：占坑未命名且超时 → 释放
	fakeNow = now.Add(3 * time.Minute)
	cl2 := newGateCluster()
	g2 := NewGate(&Budget{MaxMachines: 1, DailyBudgetUSD: 100}, map[string]machine.Provider{"team2-ci": cl2}, map[string]int{"team2-ci": 1}, map[string]float64{"team2-ci": h141})
	g2.Now = func() time.Time { return now }
	r3, ok4, _ := g2.Reserve(context.Background(), "team2-ci")
	if !ok4 {
		t.Fatal("应可占坑")
	}
	_ = r3
	g2.Now = func() time.Time { return fakeNow } // 3 分钟后
	if _, ok5, _ := g2.Reserve(context.Background(), "team2-ci"); !ok5 {
		t.Fatal("TTL 过期后占坑应释放")
	}
}

// gateCluster 模拟全局池状态：list 反映 create/delete（并发安全）——
// 现实中 provider 列表会立刻见到新机器，闸门才数得准。
type gateCluster struct {
	mu       sync.Mutex
	machines []machine.Machine
}

func newGateCluster() *gateCluster { return &gateCluster{} }

func (c *gateCluster) seed(m machine.Machine) { c.machines = append(c.machines, m) }

func (c *gateCluster) Kind() string         { return "gateCluster" }
func (c *gateCluster) Namer() machine.Namer { f := machinetest.NewFake("gate"); return f.Namer() }

func (c *gateCluster) List(ctx context.Context, tag string) ([]machine.Machine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []machine.Machine
	for _, m := range c.machines {
		for _, tg := range m.Tags {
			if tg == tag {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

func (c *gateCluster) Create(ctx context.Context, req machine.CreateRequest) (machine.Machine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := machine.Machine{Pool: "team2-ci", ID: "new", Name: req.Name, CreatedAt: time.Now(), HourlyCost: h141, Tags: req.Tags}
	c.machines = append(c.machines, m)
	return m, nil
}

func (c *gateCluster) Delete(ctx context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []machine.Machine
	for _, m := range c.machines {
		if m.ID != id {
			out = append(out, m)
		}
	}
	c.machines = out
	return nil
}

// §6 budget：按池容量拒绝（全局有余量但目标池满）。
func TestPoolCapacityRejects(t *testing.T) {
	a := machinetest.NewFake("pool-a")
	a.Seed(machine.Machine{Pool: "pool-a", ID: "1", Name: "n1", HourlyCost: 0.1, Tags: []string{"ciorch"}})
	g := NewGate(&Budget{MaxMachines: 10, DailyBudgetUSD: 100},
		map[string]machine.Provider{"pool-a": a},
		map[string]int{"pool-a": 1}, map[string]float64{"pool-a": 0.1})
	g.Now = func() time.Time { return now }
	if _, ok, reason := g.Reserve(context.Background(), "pool-a"); ok {
		t.Fatalf("池容量 1 已占用应拒绝：%s", reason)
	}
}

// §6 budget：空池无估价依据（hint 缺失）拒绝建机——不能当 0。
func TestEmptyPoolWithoutHintRejects(t *testing.T) {
	empty := machinetest.NewFake("pool-b") // 池内无机器
	g := NewGate(&Budget{MaxMachines: 10, DailyBudgetUSD: 100},
		map[string]machine.Provider{"pool-b": empty},
		map[string]int{"pool-b": 2}, map[string]float64{}) // 无 hint
	g.Now = func() time.Time { return now }
	_, ok, reason := g.Reserve(context.Background(), "pool-b")
	if ok {
		t.Fatal("空池且无 hint 应拒绝")
	}
	if !strings.Contains(reason, "不能当 0") {
		t.Fatalf("拒绝理由应说明不当 0：%s", reason)
	}
	// 有 hint 则允许（预算够）
	g.Hints["pool-b"] = 0.1
	if _, ok, reason := g.Reserve(context.Background(), "pool-b"); !ok {
		t.Fatalf("空池有 hint 应允许：%s", reason)
	}
}

// §6 budget：空池用 hint 估价、池里有机器用最近一台机器的时价。
func TestProjectionPriceNewestMachineOverHint(t *testing.T) {
	p := machinetest.NewFake("pool-a")
	p.Seed(machine.Machine{Pool: "pool-a", ID: "old", Name: "n-old", CreatedAt: now.Add(-3 * time.Hour), HourlyCost: 0.1, Tags: []string{"ciorch"}})
	p.Seed(machine.Machine{Pool: "pool-a", ID: "new", Name: "n-new", CreatedAt: now.Add(-1 * time.Hour), HourlyCost: 0.5, Tags: []string{"ciorch"}})
	// 预算只够按便宜价投影（0.1+1h≈0.6≤0.7），但池里最近一台是 0.5/h
	//（1.5h×0.5=0.75 + 0.5 > 0.7）→ 应按 0.5 投影并拒绝
	b := &Budget{MaxMachines: 10, DailyBudgetUSD: 0.7}
	g := NewGate(b, map[string]machine.Provider{"pool-a": p}, map[string]int{"pool-a": 5}, map[string]float64{"pool-a": 0.1})
	g.Now = func() time.Time { return now }
	if _, ok, _ := g.Reserve(context.Background(), "pool-a"); ok {
		t.Fatal("投影价应取池内最近一台机器的 HourlyCost（0.5），而非旧机器/hint 的 0.1")
	}
}
