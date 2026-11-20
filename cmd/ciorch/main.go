// ciorch 是 CI 执行面编排器（设计见 docs/REDESIGN.md）。
// 阶段 1 仅提供 stats 子命令：对配置仓输出排队等待/冷启代理/run 总时长基线报告。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/githubapp"
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
	fs.StringVar(&token, "token", "", "GitHub token（也可 CIORCH_TOKEN / token_file / gh auth token）")
	fs.StringVar(&repoFilter, "repo", "", "只统计该仓（owner/name，须在配置内）")
	fs.StringVar(&sinceStr, "since", "7d", "统计窗口（如 7d / 168h / 720h）")
	if err := fs.Parse(pa.subArgs); err != nil {
		return err
	}
	if pa.configPath == "" {
		pa.configPath = defaultConfigPath
	}

	switch pa.sub {
	case "stats":
		return cmdStats(pa.configPath, token, repoFilter, sinceStr)
	default:
		usage()
		return fmt.Errorf("未知子命令 %q", pa.sub)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ciorch — CI 执行面编排器（阶段 1：stats）

用法:
  ciorch [-config path] stats [-repo owner/name] [-since 7d] [-token <tok>]
`)
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
