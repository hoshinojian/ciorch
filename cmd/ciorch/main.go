// ciorch 是 CI 执行面编排器（设计见 docs/REDESIGN.md）。
// 阶段 1 仅提供 stats 子命令：对配置仓输出排队等待/冷启代理/run 总时长基线报告。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hoshinojian/ciorch/internal/budget"
	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/daemon"
	"github.com/hoshinojian/ciorch/internal/engine"
	"github.com/hoshinojian/ciorch/internal/githubapp"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/observe"
	"github.com/hoshinojian/ciorch/internal/stats"
)

const defaultConfigPath = "/etc/ciorch/ciorch.json"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ciorch:", err)
		os.Exit(1)
	}
}

// parsedArgs 是 parseArgs 的产物：全局 -config 已剥离，rest 首元素为子命令。
type parsedArgs struct {
	configPath string
	sub        string
	subArgs    []string
}

// parseArgs 解析全局 -config（允许出现在子命令前后，支持 -config=path 与
// -config path 两种形式）并定位子命令。无子命令（含只有全局 flag）返回错误，
// 不 panic——审查修正 3：`ciorch -config x` 曾 index out of range。
func parseArgs(args []string) (parsedArgs, error) {
	var configPath string
	rest := append([]string{}, args...)
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case strings.HasPrefix(a, "-config="):
			configPath = strings.TrimPrefix(a, "-config=")
			rest = append(append([]string{}, rest[:i]...), rest[i+1:]...)
		case a == "-config" && i+1 < len(rest):
			configPath = rest[i+1]
			rest = append(append([]string{}, rest[:i]...), rest[i+2:]...)
		default:
			continue
		}
		break
	}
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		return parsedArgs{}, fmt.Errorf("缺少子命令（可用：stats）")
	}
	return parsedArgs{configPath: configPath, sub: rest[0], subArgs: rest[1:]}, nil
}

func run(args []string) error {
	pa, err := parseArgs(args)
	if err != nil {
		usage()
		return err
	}

	fs := flag.NewFlagSet(pa.sub, flag.ContinueOnError)
	fs.StringVar(&pa.configPath, "config", pa.configPath, "配置文件路径（默认 "+defaultConfigPath+"）")
	var token, repoFilter, sinceStr string
	var dryRun bool
	fs.StringVar(&token, "token", "", "GitHub token（也可 CIORCH_TOKEN / token_file / gh auth token）")
	fs.StringVar(&repoFilter, "repo", "", "只统计该仓（owner/name，须在配置内）")
	fs.StringVar(&sinceStr, "since", "7d", "统计窗口（如 7d / 168h / 720h）")
	fs.BoolVar(&dryRun, "dry-run", false, "daemon：只观察与决策，不执行（shadow 模式）")
	if err := fs.Parse(pa.subArgs); err != nil {
		return err
	}
	if pa.configPath == "" {
		pa.configPath = defaultConfigPath
	}

	switch pa.sub {
	case "stats":
		return cmdStats(pa.configPath, token, repoFilter, sinceStr)
	case "plan":
		return cmdPlan(pa.configPath, token, repoFilter)
	case "status":
		return cmdStatus(pa.configPath, token, repoFilter)
	case "daemon":
		return cmdDaemon(pa.configPath, token, dryRun)
	default:
		usage()
		return fmt.Errorf("未知子命令 %q", pa.sub)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ciorch — CI 执行面编排器（阶段 2）

用法:
  ciorch [-config path] stats [-repo owner/name] [-since 7d] [-token <tok>]
  ciorch [-config path] plan   [-repo owner/name]          观察+决策，打印本轮动作（不执行）
  ciorch [-config path] status [-repo owner/name]          观察并打印各仓 Q/I/P/N 与机器
  ciorch [-config path] daemon [-dry-run]                  守护循环（-dry-run 只观察决策）
`)
}

// runtime 是 wiring 的产物（daemon/plan/status 共用）。
type runtime struct {
	gh        *githubapp.Client
	collector *observe.Collector
	engine    *engine.Engine
	providers map[string]machine.Provider
	poolCaps  map[string]int
	hints     map[string]float64
	poolOf    observe.PoolOf
}

// wiring 组装真实客户端与执行面：按配置的 pools 从 Registry 构造各池
// Provider（vpsctl 实现经 init 注册），profile→池解析 = profile.pools[0]
// （长度 1 由 config 校验强制）。
func wiring(cfg *config.Config, token string) (*runtime, error) {
	tok, err := config.ResolveToken(cfg, token, func() (string, error) {
		return githubapp.GHToken(context.Background(), "gh", nil)
	})
	if err != nil {
		return nil, err
	}
	rt := &runtime{gh: githubapp.NewClient(tok)}
	rt.providers = map[string]machine.Provider{}
	rt.poolCaps = map[string]int{}
	rt.hints = map[string]float64{}
	for name, pc := range cfg.Pools {
		p, err := machine.New(pc.Provider, name, pc.Raw, machine.Env{StateDir: cfg.StateDir})
		if err != nil {
			return nil, err
		}
		rt.providers[name] = p
		rt.poolCaps[name] = pc.MaxMachines
		if ch, ok := p.(machine.CostHinter); ok {
			rt.hints[name] = ch.HourlyCostHint()
		}
	}
	rt.poolOf = func(r config.RepoEntry, p config.Profile) observe.Pool {
		pn := cfg.PoolNameOf(p)
		return observe.Pool{Name: pn, Provider: rt.providers[pn]}
	}
	rt.collector = &observe.Collector{GH: rt.gh, PoolOf: rt.poolOf}
	rt.engine = engine.New(cfg, rt.gh, rt.providers, rt.poolOf, sshx{keyFile: cfg.SSHPrivateKeyFile}, nil, log.Default())
	rt.engine.SetCollector(rt.collector)
	return rt, nil
}

func reposOf(cfg *config.Config, repoFilter string) []config.RepoEntry {
	var out []config.RepoEntry
	for _, r := range cfg.Repos {
		if repoFilter == "" || r.Repo == repoFilter {
			out = append(out, r)
		}
	}
	return out
}

func cmdPlan(configPath, token, repoFilter string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	rt, err := wiring(cfg, token)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, r := range reposOf(cfg, repoFilter) {
		acts, stats, err := rt.engine.Reconcile(ctx, r, true) // dry-run：只决策打印
		if err != nil {
			return err
		}
		fmt.Printf("== %s Q=%d QAge=%v I=%d P=%d N=%d complete=%v，本轮动作 ==\n",
			r.Repo, stats.Q, stats.QAge, stats.I, stats.P, stats.N, stats.Complete)
		if len(acts) == 0 {
			fmt.Println("  （无）")
		}
		for _, a := range acts {
			fmt.Printf("  %-20s %s %s\n", a.Kind, a.DropletName, a.Reason)
		}
	}
	return nil
}

func cmdStatus(configPath, token, repoFilter string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	rt, err := wiring(cfg, token)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for _, r := range reposOf(cfg, repoFilter) {
		obs, err := rt.collector.Collect(ctx, r, cfg.ProfileOf(r))
		if err != nil {
			fmt.Printf("== %s 观察不完整: %v ==\n", r.Repo, err)
		}
		Q, QAge := 0, time.Duration(0)
		for _, j := range obs.Jobs {
			if j.Status == "queued" {
				Q++
				if a := obs.Now.Sub(j.CreatedAt); a > QAge {
					QAge = a
				}
			}
		}
		busy, idle, pending := 0, 0, 0
		for _, r2 := range obs.Runners {
			if r2.Status == "online" {
				if r2.Busy {
					busy++
				} else {
					idle++
				}
			}
		}
		for range obs.Machines {
			pending++
		}
		fmt.Printf("== %s ==\n  Q=%d QAge=%v 空闲runner=%d 忙runner=%d droplet=%d 未匹配排队=%d pending=%d complete=%v\n",
			r.Repo, Q, QAge, idle, busy, pending, len(obs.UnmatchedQueued), obs.PendingJobs, obs.Complete)
		pool := rt.poolOf(r, cfg.ProfileOf(r))
		fmt.Printf("  pool=%s provider=%s\n", pool.Name, pool.Provider.Kind())
		for _, d := range obs.Machines {
			fmt.Printf("  droplet %-32s 创建于 %s\n", d.Name, d.CreatedAt.Format(time.RFC3339))
		}
	}
	return nil
}

func cmdDaemon(configPath, token string, dryRun bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	rt, err := wiring(cfg, token)
	if err != nil {
		return err
	}
	b := &budget.Budget{MaxMachines: cfg.Global.MaxMachines, DailyBudgetUSD: cfg.Global.DailyBudgetUSD}
	rt.engine.Gate = budget.NewGate(b, rt.providers, rt.poolCaps, rt.hints) // 全局+池容量+预算互斥闸门
	d := daemon.New(cfg, rt.engine, b, log.Default())
	d.Usage = rt.gh.SwapUsage // 每小时用量日志：requests/304/Remaining 首尾（V3 推法）
	if dryRun {
		fmt.Println("daemon -dry-run（shadow 模式）：只观察与决策，不执行任何动作")
		d.DryRun = true // 透传到每轮 Reconcile——此前只打印提示仍是真执行（审查修正 1）
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return d.Run(ctx)
}

// sshx 经系统 ssh 子进程调机器上 cirunner act（§3.8：不维护 known_hosts、
// 机器一次性；私网优先，缺失用公网——IP 由 engine 侧选好传入）。
type sshx struct{ keyFile string }

func (s sshx) Act(ctx context.Context, ip, action string) ([]byte, error) {
	args := []string{
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
	}
	if s.keyFile != "" {
		args = append(args, "-i", s.keyFile)
	}
	args = append(args, "root@"+ip, "/usr/local/bin/cirunner", "act", action, "--json")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh %s act %s: %w", ip, action, err)
	}
	return out, nil
}

func cmdStats(configPath, token, repoFilter, sinceStr string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	since, err := parseWindow(sinceStr)
	if err != nil {
		return err
	}
	tok, err := config.ResolveToken(cfg, token, func() (string, error) {
		return githubapp.GHToken(context.Background(), "gh", nil)
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := githubapp.NewClient(tok)
	now := time.Now()
	for _, re := range cfg.Repos {
		if repoFilter != "" && re.Repo != repoFilter {
			continue
		}
		runs, err := client.ListRuns(ctx, re.Repo, now.Add(-since))
		if err != nil {
			return fmt.Errorf("%s 列 runs: %w", re.Repo, err)
		}
		var sr []stats.Run
		for _, r := range runs {
			jobs, err := client.ListJobs(ctx, re.Repo, r.ID)
			if err != nil {
				return fmt.Errorf("%s run %d 列 jobs: %w", re.Repo, r.ID, err)
			}
			sr = append(sr, stats.Run{
				ID: r.ID, RunAttempt: r.RunAttempt,
				CreatedAt: r.CreatedAt, RunStartedAt: r.RunStartedAt,
				Jobs: convertJobs(jobs),
			})
		}
		printReport(stats.Compute(re.Repo, sr), len(runs), since)
	}
	return nil
}

func convertJobs(jobs []githubapp.Job) []stats.Job {
	out := make([]stats.Job, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, stats.Job{
			ID: j.ID, Name: j.Name, CreatedAt: j.CreatedAt,
			StartedAt: j.StartedAt, CompletedAt: j.CompletedAt,
			RunnerID: j.RunnerID, RunnerName: j.RunnerName,
		})
	}
	return out
}

func printReport(rep stats.Report, runsFetched int, since time.Duration) {
	fmt.Printf("== %s（近 %s，%d 个 run）==\n", rep.Repo, since, runsFetched)
	fmt.Printf("job 开跑数 %d\n", rep.JobsStarted)
	fmt.Printf("排队等待   p50=%-8v p95=%-8v max=%v\n", rep.QueueWaitP50, rep.QueueWaitP95, rep.QueueWaitMax)
	fmt.Printf("run 总时长 p50=%-8v p95=%-8v max=%v\n", rep.RunTotalP50, rep.RunTotalP95, rep.RunTotalMax)
	fmt.Printf("冷启代理（新机器首 job 等待） n=%d avg=%v max=%v\n",
		rep.ColdStart.Samples, rep.ColdStart.Avg, rep.ColdStart.Max)
	if rep.RerunRuns > 0 || rep.RerunExcludedJobs > 0 {
		fmt.Printf("重跑 run %d（总时长按最新 attempt 起点）；因重跑排除 job %d 个（旧 attempt started_at 失真）\n",
			rep.RerunRuns, rep.RerunExcludedJobs)
	}
	names := make([]string, 0, len(rep.ByJob))
	for n := range rep.ByJob {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Println("按 job 名：")
	for _, n := range names {
		s := rep.ByJob[n]
		fmt.Printf("  %-14s n=%-3d p50=%-8v p95=%-8v max=%v\n", n, s.N, s.P50, s.P95, s.Max)
	}
}

func parseWindow(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil && days > 0 && days <= 90 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("解析 -since %q: 支持 Nd 或 Go duration", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("解析 -since %q: 窗口必须为正", s)
	}
	return d, nil
}
