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
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// Client 是 GitHub REST 客户端。
type Client struct {
	baseURL   string
	token     string // PAT 路径（App 路径为空，认证走 app）
	app       *AppAuth
	http      *http.Client
	userAgent string

	// §3.4.3：5xx/网络错误/超时同轮退避重试 2 次；ETag 条件请求；
	// 限流感知。Sleep 与退避可注入（测试）。
	sleep         func(ctx context.Context, d time.Duration) error
	bo            []time.Duration // 重试退避序列（默认 0.5s、1.5s）
	etagMu        sync.Mutex      // daemon 每仓一个 goroutine 共用本 Client
	etags         map[string]etagEntry
	rateMu        sync.Mutex
	rateRemaining int // 最近一次响应的 X-RateLimit-Remaining（-1=未知）

	// 用量统计（§11 V3：304 次数 × Remaining 实际下降量 → 304 是否扣额度）
	useMu        sync.Mutex
	useRequests  int   // 发出的 HTTP 请求数（含重试/翻页）
	useNotMod    int   // 304 响应数
	useRemainWin []int // 窗口内观察到的 Remaining 序列（首尾取自这里）
}

// UsageStats 是一个统计窗口的用量快照。
type UsageStats struct {
	Requests       int
	NotModified    int
	RemainingStart int // 窗口首个 Remaining（-1=未见）
	RemainingEnd   int // 窗口最新 Remaining（-1=未见）
}

// Usage 返回当前窗口统计（不清零）。
func (c *Client) Usage() UsageStats {
	c.useMu.Lock()
	defer c.useMu.Unlock()
	return c.usageLocked()
}

// SwapUsage 返回当前窗口统计并清零（小时日志用）。
func (c *Client) SwapUsage() UsageStats {
	c.useMu.Lock()
	defer c.useMu.Unlock()
	u := c.usageLocked()
	c.useRequests, c.useNotMod, c.useRemainWin = 0, 0, nil
	return u
}

func (c *Client) usageLocked() UsageStats {
	c.rateMu.Lock()
	remain := c.rateRemaining
	c.rateMu.Unlock()
	u := UsageStats{
		Requests:       c.useRequests,
		NotModified:    c.useNotMod,
		RemainingStart: -1,
		RemainingEnd:   remain,
	}
	if len(c.useRemainWin) > 0 {
		u.RemainingStart = c.useRemainWin[0]
		u.RemainingEnd = c.useRemainWin[len(c.useRemainWin)-1]
	}
	return u
}

// noteRequest 记一次实际发出的请求与其结果（304 与否）。
func (c *Client) noteRequest(notModified bool, remainingSeen int) {
	c.useMu.Lock()
	c.useRequests++
	if notModified {
		c.useNotMod++
	}
	if remainingSeen >= 0 {
		c.useRemainWin = append(c.useRemainWin, remainingSeen)
	}
	c.useMu.Unlock()
}

type etagEntry struct {
	etag string
	body []byte
	link string // 页间 Link rel="next"（304 复用缓存时仍可翻页）
}

// Option 定制客户端。
type Option func(*Client)

// WithBaseURL 覆盖 API 根（测试用）。
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") }
}

// WithRetryBackoff 覆盖重试退避序列（测试注入零等待）。
func WithRetryBackoff(ds ...time.Duration) Option {
	return func(c *Client) { c.bo = ds }
}

// WithSleeper 覆盖等待实现（测试）。
func WithSleeper(f func(ctx context.Context, d time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// NewAppClient 构造 GitHub App 认证客户端（每请求按仓取 installation
// token；401 作废重取一次）。
func NewAppClient(app *AppAuth, opts ...Option) *Client {
	c := &Client{
		baseURL:   defaultBaseURL,
		app:       app,
		http:      &http.Client{Timeout: 15 * time.Second},
		userAgent: "ciorch",
		bo:        []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond},
		etags:     map[string]etagEntry{},
	}
	c.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// NewClient 构造客户端（token 必填，来源解析在 internal/config.ResolveToken）。
func NewClient(token string, opts ...Option) *Client {
	c := &Client{
		baseURL:       defaultBaseURL,
		token:         token,
		http:          &http.Client{Timeout: 15 * time.Second},
		userAgent:     "ciorch",
		bo:            []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond},
		etags:         map[string]etagEntry{},
		rateRemaining: -1,
	}
	c.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// RateRemaining 返回最近一次响应的 X-RateLimit-Remaining（-1=未见）。
// daemon 据此在 <10% 时把轮询间隔 ×4（§3.4.3）。
func (c *Client) RateRemaining() int {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	return c.rateRemaining
}

// fetch 是带重试/ETag/限流的 GET（§3.4.3）。返回 body 与 Link rel="next"
// （304 时用缓存，含缓存的 link）。
func (c *Client) fetch(ctx context.Context, repo, path, fullURL string) ([]byte, string, error) {
	if fullURL == "" {
		fullURL = c.baseURL + "/" + path
	}
	backoff := append([]time.Duration(nil), c.bo...)
	c.etagMu.Lock()
	cached := c.etags[fullURL]
	c.etagMu.Unlock()
	token, terr := c.auth(ctx, repo)
	if terr != nil {
		return nil, "", fmt.Errorf("取认证 token: %w", terr)
	}

	type result struct {
		body    []byte
		link    string
		noRetry bool
	}
	res, err := withRetry(c, ctx, backoff, func() (result, bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return result{noRetry: true}, true, err
		}
		c.setHeaders(req, token)
		if cached.etag != "" {
			req.Header.Set("If-None-Match", cached.etag)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			c.noteRequest(false, -1)
			return result{}, false, err // 网络错误/超时 → 可重试
		}
		defer resp.Body.Close()
		c.noteRateLimit(resp.Header)
		rem := -1
		if v, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining")); err == nil {
			rem = v
		}
		link := linkNext(resp.Header.Get("Link"))
		if resp.StatusCode == http.StatusNotModified {
			c.noteRequest(true, rem)
			return result{body: cached.body, link: cached.link}, false, nil // 304 走缓存（无错误）
		}
		c.noteRequest(false, rem)
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if rerr != nil {
			return result{}, false, rerr
		}
		if resp.StatusCode >= 500 {
			return result{}, false, fmt.Errorf("HTTP %d", resp.StatusCode) // 可重试
		}
		if resp.StatusCode == http.StatusUnauthorized && c.app != nil {
			// §3.4.2：收到 401 立即作废该仓 token，重取一次（退避后重试）
			c.app.Invalidate(repo)
			if nt, nerr := c.auth(ctx, repo); nerr == nil {
				token = nt
			}
			return result{}, false, fmt.Errorf("401（已作废重取，将重试）")
		}
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			// 403/429 带 Retry-After：遵守后重试一次
			if ra := parseRetryAfter(resp.Header.Get("Retry-After")); ra > 0 {
				if serr := c.sleep(ctx, ra); serr != nil {
					return result{noRetry: true}, true, serr
				}
				return result{}, false, fmt.Errorf("限流（Retry-After=%v 已等待）", ra)
			}
			return result{noRetry: true}, true, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			return result{noRetry: true}, true, fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode) // 4xx 不重试
		}
		if et := resp.Header.Get("ETag"); et != "" {
			c.etagMu.Lock()
			c.etags[fullURL] = etagEntry{etag: et, body: raw, link: link}
			c.etagMu.Unlock()
		}
		return result{body: raw, link: link}, true, nil
	})
	if err != nil {
		return nil, "", err
	}
	return res.body, res.link, nil
}

// withRetry：可重试错误（网络/5xx/限流）退避重试；noRetry 直接返回。
func withRetry[T any](c *Client, ctx context.Context, backoff []time.Duration, attempt func() (T, bool, error)) (T, error) {
	var zero T
	res, noRetry, err := attempt()
	if err == nil {
		return res, nil
	}
	if noRetry {
		return zero, err
	}
	for _, d := range backoff {
		if serr := c.sleep(ctx, d); serr != nil {
			return zero, serr
		}
		res, noRetry, err = attempt()
		if err == nil {
			return res, nil
		}
		if noRetry {
			return zero, err
		}
	}
	return zero, err
}

func (c *Client) noteRateLimit(h http.Header) {
	v := h.Get("X-RateLimit-Remaining")
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return
	}
	c.rateMu.Lock()
	c.rateRemaining = n
	c.rateMu.Unlock()
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// Run 是 workflow run 的最小事实（完整观察模型见阶段 2 internal/observe）。
type Run struct {
	ID           int64     `json:"id"`
	Status       string    `json:"status"`
	RunAttempt   int       `json:"run_attempt"`
	CreatedAt    time.Time `json:"created_at"`
	RunStartedAt time.Time `json:"run_started_at"`
}

// Job 是 jobs 接口条目（阶段 1-2 字段：排队等待/冷启代理/label 匹配所需）。
type Job struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	RunnerID    int64     `json:"runner_id"`
	RunnerName  string    `json:"runner_name"`
	Labels      []string  `json:"labels"`
}

// RunnerLabel 是 runners API 的 label 条目——**对象不是字符串**
// （shadow 实录 2026-11-20：`{"id":1,"name":"self-hosted"}`，此前按 []string
// 解析，一有 runner 注册 runners 路即失败）。
type RunnerLabel struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Runner 是 runners 接口条目（本仓全部注册，含外部——归属过滤由 observe/decide 做）。
type Runner struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Status string        `json:"status"` // online | offline
	Busy   bool          `json:"busy"`
	Labels []RunnerLabel `json:"labels"`
}

// LabelNames 取 label 名列表（observe 侧消费字符串）。
func (r Runner) LabelNames() []string {
	out := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		out = append(out, l.Name)
	}
	return out
}

// ListRuns 返回 since 之后创建的全部 run（按 created>= 服务端过滤 + 客户端兜底）。
func (c *Client) ListRuns(ctx context.Context, repo string, since time.Time) ([]Run, error) {
	var out []Run
	q := url.Values{}
	q.Set("per_page", "100")
	q.Set("created", ">="+since.UTC().Format(time.RFC3339))
	err := c.paginate(ctx, repo, fmt.Sprintf("repos/%s/actions/runs", repo), q, func(body json.RawMessage) error {
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
	err := c.paginate(ctx, repo, fmt.Sprintf("repos/%s/actions/runs/%d/jobs", repo, runID), q, func(body json.RawMessage) error {
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

// MintToken 铸 runner registration token（1h 时效，建机瞬间现铸现用，§2.3）。
func (c *Client) MintToken(ctx context.Context, repo string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, repo, http.MethodPost, fmt.Sprintf("repos/%s/actions/runners/registration-token", repo), &out); err != nil {
		return "", fmt.Errorf("铸 registration token: %w", err)
	}
	if len(out.Token) < 16 {
		return "", fmt.Errorf("registration token 异常（长度 %d）", len(out.Token))
	}
	return out.Token, nil
}

// RemoveRunner 删除 runner 注册（Drain 第一步/清残留，§3.8）。
func (c *Client) RemoveRunner(ctx context.Context, repo string, runnerID int64) error {
	return c.do(ctx, repo, http.MethodDelete, fmt.Sprintf("repos/%s/actions/runners/%d", repo, runnerID), nil)
}

// do 是非 GET 端点的薄封装（无分页；App 路径 401 作废重取一次）。
func (c *Client) do(ctx context.Context, repo, method, path string, out any) error {
	token, terr := c.auth(ctx, repo)
	if terr != nil {
		return fmt.Errorf("取认证 token: %w", terr)
	}
	var body []byte
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+path, nil)
		if err != nil {
			return err
		}
		c.setHeaders(req, token)
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("%s %s: %w", method, path, err)
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			return rerr
		}
		if resp.StatusCode == http.StatusUnauthorized && c.app != nil && attempt == 0 {
			// §3.4.2：401 立即作废该仓 token，重取一次
			c.app.Invalidate(repo)
			if nt, nerr := c.auth(ctx, repo); nerr == nil {
				token = nt
				continue
			}
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
		}
		body = raw
		break
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

// auth 取该仓的认证 token（App 路径按仓铸 installation token；PAT 直接用）。
func (c *Client) auth(ctx context.Context, repo string) (string, error) {
	if c.app != nil {
		return c.app.Token(ctx, repo)
	}
	return c.token, nil
}

func (c *Client) setHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.userAgent)
}

// ListRunners 返回仓内全部 runner 注册（含 labels，§3.4.4）。
func (c *Client) ListRunners(ctx context.Context, repo string) ([]Runner, error) {
	var out []Runner
	q := url.Values{}
	q.Set("per_page", "100")
	err := c.paginate(ctx, repo, fmt.Sprintf("repos/%s/actions/runners", repo), q, func(body json.RawMessage) error {
		var page struct {
			Runners []Runner `json:"runners"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("解析 runners: %w", err)
		}
		out = append(out, page.Runners...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// paginate 逐页请求并回调解析，跟随 Link rel="next"（上限 20 页防御）。
// 请求统一走 fetch（§3.4.3 重试/ETag/限流）。
func (c *Client) paginate(ctx context.Context, repo string, path string, q url.Values, parse func(json.RawMessage) error) error {
	next := c.baseURL + "/" + path + "?" + q.Encode()
	for i := 0; i < 20 && next != ""; i++ {
		body, link, err := c.fetch(ctx, repo, path, next)
		if err != nil {
			return err
		}
		if err := parse(body); err != nil {
			return err
		}
		next = link
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
