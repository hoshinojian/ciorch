// Package budget 是全局配额与每日预算（§3.7）：钱有硬上限，由确定性代码执行。
package budget

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hoshinojian/ciorch/internal/vpsctlapi"
)

// dropletsPerMonth 封顶小时数（DO 月封顶 672 小时口径，§3.7）。
const hoursPerMonth = 672.0

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

	// spentToday 累计当日已删机器的花费（活机按 droplet.CreatedAt 现算，
	// 死机要靠删除时入账，否则删得越快越"省钱"）。
	spentToday float64
	dayUTC     string
}

// Lister 是全局列机能力（vpsctlapi 实现）。
type Lister interface {
	ListByTag(ctx context.Context, tag, account string) ([]vpsctlapi.Droplet, error)
}

// GlobalCount 统计编排器名下全部 droplet：按配置里的账号列 -tag ciorch，
// 同 token 多账号会同机重复——按 droplet id 去重（原型已踩过）。
// 编排器自身机器用 tag ciorch-host，不匹配公共 tag ciorch，不计入（§3.7）。
func (b *Budget) GlobalCount(ctx context.Context, lister Lister, accounts []string) (int, error) {
	seen := map[string]bool{}
	for _, acc := range accounts {
		ds, err := lister.ListByTag(ctx, "ciorch", acc)
		if err != nil {
			return 0, fmt.Errorf("账号 %s 列机: %w", acc, err)
		}
		for _, d := range ds {
			seen[d.ID] = true
		}
	}
	return len(seen), nil
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

// SpentToday 当日花费：活机 Σ max(0, 当日存活小时) × hourly + 已删机器入账。
func (b *Budget) SpentToday(now time.Time, live []vpsctlapi.Droplet) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	total := b.spentToday
	for _, d := range live {
		from := d.CreatedAt
		if dayStart.After(from) {
			from = dayStart
		}
		hours := now.Sub(from).Hours()
		if hours < 0 {
			continue
		}
		total += hours * d.PriceMonthly / hoursPerMonth
	}
	return total
}

// RecordDeletion 删除机器时入账当日剩余时段的花费（避免"删得快=没花钱"）。
func (b *Budget) RecordDeletion(now time.Time, d vpsctlapi.Droplet) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if d.CreatedAt.Before(dayStart) {
		d.CreatedAt = dayStart
	}
	b.spentToday += now.Sub(d.CreatedAt).Hours() * d.PriceMonthly / hoursPerMonth
}

// CanCreate 判定再建一台（价 priceMonthly）是否越线；返回false 即场景 F。
func (b *Budget) CanCreate(now time.Time, globalCount int, live []vpsctlapi.Droplet, priceMonthly float64) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollover(now)
	if globalCount >= b.MaxDroplets {
		return false, fmt.Sprintf("全局配额已满（%d/%d）", globalCount, b.MaxDroplets)
	}
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	total := b.spentToday
	for _, d := range live {
		from := d.CreatedAt
		if dayStart.After(from) {
			from = dayStart
		}
		if h := now.Sub(from).Hours(); h > 0 {
			total += h * d.PriceMonthly / hoursPerMonth
		}
	}
	// 预计建一台后：以"新机至少跑 1 小时"为最小计费单位估（满 24h 投影会让
	// §3.3 示例的 $5 日预算永远建不了 c2 机型，与配置矛盾；口径已在 PR 标注
	// 待确认，宁可紧不可松的方向由 RecordDeletion/SpentToday 真实入账兜底）。
	projected := total + priceMonthly/hoursPerMonth
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
type Gate struct {
	mu       sync.Mutex
	B        *Budget
	Lister   Lister
	Accounts []string
	// Now 可注入时钟（测试）；默认 time.Now。
	Now func() time.Time
	// pending 是在途占坑（Reserve 计入、Commit 后携带 droplet 名保留到
	// 列表可见/TTL——vpsctl list 见到新机有秒级延迟，过早释放会让别的仓
	// 在窗口内多占一个名额）。
	pending []gatePending
}

// NewGate 构造。
func NewGate(b *Budget, l Lister, accounts []string) *Gate {
	return &Gate{B: b, Lister: l, Accounts: accounts, Now: time.Now}
}

// Reservation 是一次占坑句柄：建机成功后 Commit（携带 droplet 名，占坑
// 保留到该机器出现在全局列表里或 TTL 到期——vpsctl list 见到新机有秒级
// 延迟，过早释放会让别的仓在窗口内多占一个名额）；失败 Abort。
type Reservation struct {
	id int
}

type gatePending struct {
	name string // 建机成功后的 droplet 名（空 = 未完成）
	at   time.Time
}

// gateTTL 是未出现在列表里的占坑最长保留时间（保守兜底）。
const gateTTL = 2 * time.Minute

// Reserve 互斥占坑（复审修正 8 的 TOCTOU 收口）：判定配额+预算并计入
// 在途，建机调用方成功后 Commit、失败后 Abort。价 price=0 时用全局在册价近似。
func (g *Gate) Reserve(ctx context.Context, price float64) (*Reservation, bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	live := map[string]vpsctlapi.Droplet{}
	for _, acc := range g.Accounts {
		ds, err := g.Lister.ListByTag(ctx, "ciorch", acc)
		if err != nil {
			return nil, false, fmt.Sprintf("全局列机失败（保守拒绝）: %v", err)
		}
		for _, d := range ds {
			live[d.ID] = d
		}
	}
	var liveList []vpsctlapi.Droplet
	for _, d := range live {
		liveList = append(liveList, d)
	}
	// 清扫：TTL 过期的占坑释放；已命名且出现在列表里的释放（名额由 list 计）
	kept := g.pending[:0]
	for _, pnd := range g.pending {
		if pnd.name != "" {
			if _, visible := liveByName(live, pnd.name); visible {
				continue // 已可见：释放占坑
			}
		}
		if g.Now().Sub(pnd.at) > gateTTL {
			continue // TTL 兜底释放
		}
		kept = append(kept, pnd)
	}
	g.pending = kept
	count := len(live) + len(g.pending)
	if price == 0 && len(liveList) > 0 {
		price = liveList[0].PriceMonthly // 同 profile 近似价
	}
	ok, reason := g.B.CanCreate(g.Now(), count, liveList, price)
	if !ok {
		return nil, false, reason
	}
	g.pending = append(g.pending, gatePending{at: g.Now()})
	return &Reservation{id: len(g.pending) - 1}, true, ""
}

// Commit 建机成功：携带 droplet 名占坑保留至列表可见/TTL。
func (g *Gate) Commit(r *Reservation, dropletName string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r != nil && r.id < len(g.pending) {
		g.pending[r.id].name = dropletName
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

func liveByName(live map[string]vpsctlapi.Droplet, name string) (vpsctlapi.Droplet, bool) {
	for _, d := range live {
		if d.Name == name {
			return d, true
		}
	}
	return vpsctlapi.Droplet{}, false
}

// RecordDeletion 删机入账（互斥）。
func (g *Gate) RecordDeletion(now time.Time, price float64, created time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.B.RecordDeletion(now, vpsctlapi.Droplet{PriceMonthly: price, CreatedAt: created})
}
