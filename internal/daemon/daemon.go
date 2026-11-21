// Package daemon 是守护循环（§3.9）：每仓独立定时器，间隔按活跃/空闲动态
// 切换；每轮 observe → decide → budget → 执行 → 原子写状态；启动取 flock
// 防双开。webhook 触发（2s 防抖）在阶段 4 接入——Trigger 即为其入口。
package daemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/hoshinojian/ciorch/internal/githubapp"

	"github.com/hoshinojian/ciorch/internal/budget"
	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/engine"
	"github.com/hoshinojian/ciorch/internal/state"
)

// 默认轮询间隔（§3.6.6；global.tuning 覆盖在 config 侧解析，此处接口注入）。
const (
	defaultPollActive = 15 * time.Second
	defaultPollIdle   = 60 * time.Second
)

// Clock 注入时间（测试）。
type Clock func() time.Time

// persisted 是 state.json 的落盘形态（决策记忆 + 当日预算累计）。
type persisted struct {
	Repos  map[string]decide.RepoState `json:"repos"`
	Budget *budget.BudgetSnapshot      `json:"budget,omitempty"`
}

// Daemon 是守护进程。
type Daemon struct {
	Cfg                  *config.Config
	Engine               *engine.Engine
	Budget               *budget.Budget
	Log                  *log.Logger
	PollActive, PollIdle time.Duration
	Now                  Clock
	// DryRun = shadow 模式：只观察与决策，不执行任何动作（§3.8）。
	DryRun bool
	// Usage 返回并清零一小时用量统计（nil = 不打小时日志）。
	Usage func() githubapp.UsageStats

	mu        sync.Mutex
	triggers  map[string]chan struct{}
	lastStats map[string]engine.RoundStats // 仓 → 最近一轮 Q 等（active 判定与摘要）

	// lastOK 是最近一次全仓 reconcile 成功时刻（/healthz 判据）。
	lastOK time.Time
	// Handler 是 HTTP 面（webhook/healthz/status）；nil = 不起 HTTP。
	Handler http.Handler
	// ListenAddr 是 HTTP 监听地址（只监听 127.0.0.1，公网由 Caddy 反代）。
	ListenAddr string
}

// New 构造。
func New(cfg *config.Config, e *engine.Engine, b *budget.Budget, lg *log.Logger) *Daemon {
	if lg == nil {
		lg = log.Default()
	}
	return &Daemon{
		Cfg: cfg, Engine: e, Budget: b, Log: lg,
		PollActive: defaultPollActive, PollIdle: defaultPollIdle,
		Now:       time.Now,
		triggers:  map[string]chan struct{}{},
		lastStats: map[string]engine.RoundStats{},
	}
}

// Trigger 请求某仓立即 reconcile（webhook 入口，阶段 4 接入；重复触发合并）。
func (d *Daemon) Trigger(repo string) {
	d.mu.Lock()
	ch := d.triggers[repo]
	d.mu.Unlock()
	if ch != nil {
		select {
		case ch <- struct{}{}:
		default: // 已有待处理的触发，合并
		}
	}
}

// Run 主循环；ctx 取消即退出。
func (d *Daemon) Run(ctx context.Context) error {
	// HTTP 面（§3.11）：webhook 触发 reconcile（防抖由 Trigger 合并）、
	// healthz/status 只读——dry-run 下同样零写入（webhook 只入缓存+触发）。
	if d.Handler != nil && d.ListenAddr != "" {
		srv := &http.Server{Addr: d.ListenAddr, Handler: d.Handler}
		ln, err := net.Listen("tcp", d.ListenAddr)
		if err != nil {
			return fmt.Errorf("HTTP 监听 %s: %w", d.ListenAddr, err)
		}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				d.Log.Printf("HTTP 服务退出: %v", err)
			}
		}()
		d.Log.Printf("HTTP 监听 %s（/webhook /healthz /status）", d.ListenAddr)
	}

	lockPath := filepath.Join(d.Cfg.StateDir, "ciorch.lock")
	unlock, err := state.Lock(lockPath)
	if err != nil {
		return err
	}
	defer unlock()

	// 恢复决策记忆与当日预算累计
	var p persisted
	statePath := filepath.Join(d.Cfg.StateDir, "state.json")
	if _, err := state.Load(statePath, &p); err != nil {
		d.Log.Printf("恢复状态失败（按全新启动）: %v", err)
	}
	if p.Repos != nil {
		d.Engine.States = p.Repos
	}
	// 预算当日累计恢复（守护重启不清零——复审修正：此前只在内存）
	if p.Budget != nil {
		d.Budget.Restore(d.Now(), *p.Budget)
		d.Log.Printf("恢复当日预算累计：%s 已花 %.2f USD", p.Budget.DayUTC, p.Budget.SpentToday)
	}
	// 每小时打印限流余量（shadow 观察项：24h API 健壮性）
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if d.Usage != nil {
					u := d.Usage()
					d.Log.Printf("api-usage 1h: requests=%d 304=%d remaining %d→%d",
						u.Requests, u.NotModified, u.RemainingStart, u.RemainingEnd)
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for _, r := range d.Cfg.Repos {
		ch := make(chan struct{}, 1)
		d.mu.Lock()
		d.triggers[r.Repo] = ch
		d.mu.Unlock()
		wg.Add(1)
		go func(repo config.RepoEntry, trigger chan struct{}) {
			defer wg.Done()
			d.loopRepo(ctx, repo, trigger, statePath)
		}(r, ch)
	}
	wg.Wait()
	return nil
}

func (d *Daemon) loopRepo(ctx context.Context, repo config.RepoEntry, trigger chan struct{}, statePath string) {
	// 启动即立即 reconcile 一次（§3.9）
	next := d.round(ctx, repo, statePath)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		case <-trigger:
		}
		next = d.round(ctx, repo, statePath)
	}
}

// round 一轮：reconcile（带全局建机配额）→ 摘要日志 → 持久化 → 返回下轮间隔。
func (d *Daemon) round(ctx context.Context, repo config.RepoEntry, statePath string) time.Duration {
	now := d.Now()

	acts, stats, err := d.Engine.Reconcile(ctx, repo, d.DryRun)
	if err != nil {
		d.Log.Printf("[%s] reconcile 失败: %v", repo.Repo, err)
	}
	d.mu.Lock()
	d.lastStats[repo.Repo] = stats
	if obs2, oerr := d.Engine.Collector.Collect(ctx, repo, d.Cfg.ProfileOf(repo)); oerr == nil && obs2.Complete {
		d.lastOK = now // 一轮观察完整即视为健康（healthz 判据）
	}
	d.mu.Unlock()

	// 摘要一行（§3.9：repo=… Q=… QAge=… I=… P=… N=… actions=[…] complete=…）
	d.Log.Printf("[%s] Q=%d QAge=%v I=%d P=%d N=%d actions=[%s] complete=%v",
		repo.Repo, stats.Q, stats.QAge, stats.I, stats.P, stats.N, kindsOf(acts), stats.Complete)

	// 原子写状态（锁内深拷贝快照 + 当日预算累计）
	snap := d.Budget.Snapshot(d.Now())
	if err := state.Save(statePath, persisted{Repos: d.Engine.StatesSnapshot(), Budget: &snap}); err != nil {
		d.Log.Printf("[%s] 状态持久化失败: %v", repo.Repo, err)
	}
	return d.nextInterval(repo, now)
}

// active 判定（§3.6.6 poll_active 的条件）：Q>0、冷启在途、有 busy runner
// 或有未结事故。观察数据拿不到"未结事故"（阶段 5），此处按前三者 + 状态里
// 有机器处于 remedy。
func (d *Daemon) active(repo config.RepoEntry) bool {
	d.mu.Lock()
	stats := d.lastStats[repo.Repo]
	d.mu.Unlock()
	if stats.Q > 0 { // 有排队：活跃档（复审修正：此前只看机器状态）
		return true
	}
	st := d.Engine.StatesSnapshot()[repo.Repo]
	for _, m := range st.Machines {
		if m.Remedy != "" {
			return true
		}
		if !m.BusySince.IsZero() {
			return true
		}
		if m.FirstOnlineAt.IsZero() {
			return true // 冷启在途
		}
	}
	return false
}

func (d *Daemon) nextInterval(repo config.RepoEntry, now time.Time) time.Duration {
	dur := d.PollIdle
	if d.active(repo) {
		dur = d.PollActive
	}
	return dur
}

// kindsOf 动作名列表（摘要行用）。
func kindsOf(acts []decide.Action) string {
	kinds := ""
	for _, a := range acts {
		if kinds != "" {
			kinds += ","
		}
		kinds += string(a.Kind)
	}
	if kinds == "" {
		kinds = "无"
	}
	return kinds
}
