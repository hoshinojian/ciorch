// Package budget 是全局配额与每日预算（§3.7）：钱有硬上限，由确定性代码执行。
// 成本口径（实施计划 §3.4）：Machine.HourlyCost 由提供商给出（vpsctl 按
// price_monthly/672 折算），本包不再出现月价与 672 的换算。
package budget

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hoshinojian/ciorch/internal/machine"
)

// Request 是一个仓的建机请求（多仓争抢时按 QAge 从老到新发放）。
type Request struct {
	Repo string
	QAge time.Duration
	Want int
}

// Allocation 是分配结果：仓 → 本轮允许建机数。
type Allocation map[string]int

// Budget 维护全局配额与当日（UTC）花费。
type Budget struct {
	mu             sync.Mutex
	MaxDroplets    int
	DailyBudgetUSD float64

	// spentToday 累计当日已删机器的花费（活机按 Machine.CreatedAt 现算，
	// 死机要靠删除时入账，否则删得越快越"省钱"）。
	spentToday float64
	dayUTC     string
}

// Snapshot 导出当日累计（持久化用——此前只在内存，守护重启当日花费清零）。
type BudgetSnapshot struct {
	DayUTC     string  `json:"day_utc"`
	SpentToday float64 `json:"spent_today"`
}

func (b *Budget) Snapshot(now time.Time) BudgetSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	return BudgetSnapshot{DayUTC: b.dayUTC, SpentToday: b.spentToday}
}

// Restore 恢复当日累计（同日才恢复；跨日自然清零）。
func (b *Budget) Restore(now time.Time, snap BudgetSnapshot) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	if snap.DayUTC == b.dayUTC {
		b.spentToday = snap.SpentToday
	}
}

// rollover 跨 UTC 自然日清零（§3.7）。
func (b *Budget) rollover(now time.Time) {
	day := now.UTC().Format("2006-01-02")
	if b.dayUTC != day {
		b.dayUTC = day
		b.spentToday = 0
	}
}

// SpentToday 当日花费：活机 Σ max(0, 当日存活小时) × HourlyCost + 已删机器入账。
func (b *Budget) SpentToday(now time.Time, live []machine.Machine) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	total := b.spentToday
	for _, m := range live {
		from := m.CreatedAt
		if dayStart.After(from) {
			from = dayStart
		}
		hours := now.Sub(from).Hours()
		if hours < 0 {
			continue
		}
		total += hours * m.HourlyCost
	}
	return total
}

// RecordDeletion 删除机器时入账当日剩余时段的花费（避免"删得快=没花钱"）。
// hourly 是该机的每小时成本（提供商口径）。
func (b *Budget) RecordDeletion(now time.Time, hourly float64, created time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if created.Before(dayStart) {
		created = dayStart
	}
	b.spentToday += now.Sub(created).Hours() * hourly
}

// CanCreate 判定再建一台（每小时成本 hourly）是否越线；返回 false 即场景 F。
// 预计口径："新机至少跑 1 小时"（满 24h 投影会让示例的 $5 日预算永远建不了
// c2 机型；真实花费由 SpentToday/RecordDeletion 入账兜底）。
func (b *Budget) CanCreate(now time.Time, globalCount int, live []machine.Machine, hourly float64) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	if globalCount >= b.MaxDroplets {
		return false, fmt.Sprintf("全局配额已满（%d/%d）", globalCount, b.MaxDroplets)
	}
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	total := b.spentToday
	for _, m := range live {
		from := m.CreatedAt
		if dayStart.After(from) {
			from = dayStart
		}
		if h := now.Sub(from).Hours(); h > 0 {
			total += h * m.HourlyCost
		}
	}
	projected := total + hourly
	if projected > b.DailyBudgetUSD {
		return false, fmt.Sprintf("预计 %.2f 超日预算 %.2f", projected, b.DailyBudgetUSD)
	}
	return true, ""
}

// Allocate 多仓争抢分配：按 QAge 从老到新发放配额（§3.7）。
func Allocate(globalCount, maxDroplets int, reqs []Request) Allocation {
	out := Allocation{}
	remaining := maxDroplets - globalCount
	sort.SliceStable(reqs, func(i, j int) bool { return reqs[i].QAge > reqs[j].QAge })
	for _, r := range reqs {
		n := r.Want
		if n > remaining {
			n = remaining
		}
		if n < 0 {
			n = 0
		}
		out[r.Repo] = n
		remaining -= n
	}
	return out
}

// Gate 是全局建机闸门（复审修正 8）：把"全局配额 + 每日预算判定 + 删机入账"
// 放进同一互斥区间——多仓并发时不会同时抢掉最后一个名额，也不会漏记账。
// 全局列机改为对每个池 List(公共标记) 后按 (池, ID) 去重（实施计划 §3.4）。
type Gate struct {
	mu sync.Mutex
	B  *Budget
	// Providers 是全部池（池名 → Provider）；公共标记列机按池聚合。
	Providers map[string]machine.Provider
	// Now 可注入时钟（测试）；默认 time.Now。
	Now func() time.Time
	// pending 是在途占坑（Reserve 计入、Commit 后携带机器名保留到
	// 列表可见/TTL——提供商列表见到新机有秒级延迟，过早释放会让别的仓
	// 在窗口内多占一个名额）。
	pending []gatePending
}

// NewGate 构造。
func NewGate(b *Budget, providers map[string]machine.Provider) *Gate {
	return &Gate{B: b, Providers: providers, Now: time.Now}
}

// Reservation 是一次占坑句柄：建机成功后 Commit（携带机器名，占坑
// 保留到该机器出现在全局列表里或 TTL 到期）；失败 Abort。
type Reservation struct {
	id int
}

type gatePending struct {
	name string // 建机成功后的机器名（空 = 未完成）
	at   time.Time
}

// gateTTL 是未出现在列表里的占坑最长保留时间（保守兜底）。
const gateTTL = 2 * time.Minute

// liveMachines 对每个池按公共标记列机，按 (池, ID) 去重，返回
// （池名排序、池内 ID 排序的）确定性列表。
func (g *Gate) liveMachines(ctx context.Context) ([]machine.Machine, error) {
	pools := make([]string, 0, len(g.Providers))
	for name := range g.Providers {
		pools = append(pools, name)
	}
	sort.Strings(pools)
	live := map[string]machine.Machine{} // key = pool + "\x00" + ID
	for _, pool := range pools {
		ms, err := g.Providers[pool].List(ctx, "ciorch")
		if err != nil {
			return nil, fmt.Errorf("池 %s 列机（保守拒绝）: %w", pool, err)
		}
		for _, m := range ms {
			live[pool+"\x00"+m.ID] = m
		}
	}
	keys := make([]string, 0, len(live))
	for k := range live {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]machine.Machine, 0, len(keys))
	for _, k := range keys {
		out = append(out, live[k])
	}
	return out, nil
}

// Reserve 互斥占坑（复审修正 8 的 TOCTOU 收口）：判定配额+预算并计入
// 在途，建机调用方成功后 Commit、失败后 Abort。hourly=0 时用全局在册价近似。
func (g *Gate) Reserve(ctx context.Context, hourly float64) (*Reservation, bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	liveList, err := g.liveMachines(ctx)
	if err != nil {
		return nil, false, err.Error()
	}
	liveByName := map[string]bool{}
	for _, m := range liveList {
		liveByName[m.Name] = true
	}
	// 清扫：TTL 过期的占坑释放；已命名且出现在列表里的释放（名额由 list 计）
	kept := g.pending[:0]
	for _, pnd := range g.pending {
		if pnd.name != "" {
			if liveByName[pnd.name] {
				continue // 已可见：释放占坑
			}
		}
		if g.Now().Sub(pnd.at) > gateTTL {
			continue // TTL 兜底释放
		}
		kept = append(kept, pnd)
	}
	g.pending = kept
	count := len(liveList) + len(g.pending)
	if hourly == 0 && len(liveList) > 0 {
		hourly = liveList[0].HourlyCost // 同 profile 近似价
	}
	ok, reason := g.B.CanCreate(g.Now(), count, liveList, hourly)
	if !ok {
		return nil, false, reason
	}
	g.pending = append(g.pending, gatePending{at: g.Now()})
	return &Reservation{id: len(g.pending) - 1}, true, ""
}

// Commit 建机成功：携带机器名占坑保留至列表可见/TTL。
func (g *Gate) Commit(r *Reservation, machineName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r != nil && r.id < len(g.pending) {
		g.pending[r.id].name = machineName
	}
}

// Abort 建机失败：立即释放占坑。
func (g *Gate) Abort(r *Reservation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r != nil && r.id < len(g.pending) {
		g.pending[r.id].at = time.Time{} // 立即过期，下次 Reserve 清扫
	}
}

// RecordDeletion 删机入账（互斥；hourly 为提供商口径的每小时成本）。
func (g *Gate) RecordDeletion(now time.Time, hourly float64, created time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.B.RecordDeletion(now, hourly, created)
}
