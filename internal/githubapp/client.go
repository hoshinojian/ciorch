// Package githubapp 是 GitHub REST 客户端（阶段 1：PAT / gh auth token 临时
// token 源；GitHub App JWT + installation token 在阶段 3 落地，见
// docs/REDESIGN.md §3.4/§9）。只做只读观察：runs 列表（created>= 过滤）与
// 逐 run jobs（filter=latest）。分页跟随 Link rel="next"，per_page=100。
//
// 判据口径（P141）：run 级 status 不可靠，一切需求判定以 jobs 接口为准；
// 本包只取事实，过滤/判定归 internal/stats 与（阶段 2）internal/observe。
package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// Client 是 GitHub REST 客户端。
type Client struct {
	baseURL   string
	token     string
	http      *http.Client
	userAgent string
}

// Option 定制客户端。
type Option func(*Client)

// WithBaseURL 覆盖 API 根（测试用）。
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// NewClient 构造客户端（token 必填，来源解析在 internal/config.ResolveToken）。
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		baseURL:   defaultBaseURL,
		token:     token,
		http:      &http.Client{Timeout: 15 * time.Second},
		userAgent: "ciorch",
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Run 是 workflow run 的最小事实（完整观察模型见阶段 2 internal/observe）。
type Run struct {
	ID           int64     `json:"id"`
	Status       string    `json:"status"`
	RunAttempt   int       `json:"run_attempt"`
	CreatedAt    time.Time `json:"created_at"`
	RunStartedAt time.Time `json:"run_started_at"`
}

// Job 是 jobs 接口条目（阶段 1 字段：排队等待与冷启代理所需）。
type Job struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	RunnerID    int64     `json:"runner_id"`
	RunnerName  string    `json:"runner_name"`
}

// ListRuns 返回 since 之后创建的全部 run（按 created>= 服务端过滤 + 客户端兜底）。
func (c *Client) ListRuns(ctx context.Context, repo string, since time.Time) ([]Run, error) {
	var out []Run
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("created", ">="+since.UTC().Format(time.RFC3339))
	err := c.paginate(ctx, fmt.Sprintf("repos/%s/actions/runs", repo), q, func(body json.RawMessage) error {
		var page struct {
			WorkflowRuns []Run `json:"workflow_runs"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("解析 runs: %w", err)
		}
		out = append(out, page.WorkflowRuns...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 客户端兜底过滤（API 对 created>= 的边界行为不承诺）。
	filtered := out[:0]
	for _, r := range out {
		if !r.CreatedAt.Before(since) {
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
}

// ListJobs 返回一个 run 的全部 job（filter=latest，忽略 re-run 旧 attempt）。
func (c *Client) ListJobs(ctx context.Context, repo string, runID int64) ([]Job, error) {
	var out []Job
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("filter", "latest")
	err := c.paginate(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/jobs", repo, runID), q, func(body json.RawMessage) error {
		var page struct {
			Jobs []Job `json:"jobs"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("解析 jobs: %w", err)
		}
		out = append(out, page.Jobs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// paginate 逐页请求并回调解析，跟随 Link rel="next"（上限 20 页防御）。
func (c *Client) paginate(ctx context.Context, path string, q url.Values, parse func(json.RawMessage) error) error {
	next := c.baseURL + "/" + path + "?" + q.Encode()
	for i := 0; i < 20 && next != ""; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", c.userAgent)

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("GET %s: %w", path, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		closeErr := resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
		}
		if readErr != nil {
			return fmt.Errorf("读取 %s 响应: %w", path, readErr)
		}
		if closeErr != nil {
			return fmt.Errorf("关闭 %s 响应: %w", path, closeErr)
		}
		if err := parse(body); err != nil {
			return err
		}
		next = linkNext(resp.Header.Get("Link"))
	}
	return nil
}

// linkNext 从 Link 头取 rel="next" 的 URL（无则空串）。
func linkNext(header string) string {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		urlPart, params, ok := strings.Cut(part, ";")
		if !ok {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			if strings.TrimSpace(p) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(urlPart), "<>")
			}
		}
	}
	return ""
}

// GHToken 经 gh CLI 子进程取当前登录 token（run 可注入便于测试）。
func GHToken(ctx context.Context, ghBin string, run func(ctx context.Context, name string, args ...string) ([]byte, error)) (string, error) {
	if run == nil {
		run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		}
	}
	out, err := run(ctx, ghBin, "auth", "token")
	if err != nil {
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if len(tok) < 16 {
		return "", fmt.Errorf("gh auth token 输出异常（长度 %d）", len(tok))
	}
	return tok, nil
}
