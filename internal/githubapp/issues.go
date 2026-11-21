// 事故 issue 读写（REDESIGN §3.10.2）：开/评/打标签/关/重开、标签补建。
// 本文件只新增方法与一个带请求体的内部封装，不改 client.go（认证/token
// 部分由并行 session 维护）。
package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Issue 是仓库 issue 的最小视图（事故模块够用）。
type Issue struct {
	Number  int64  `json:"number"`
	Title   string `json:"title"`
	State   string `json:"state"` // open | closed
	HTMLURL string `json:"html_url"`
}

// labelColor 是补建标签用的固定颜色（设计未指定，取 GitHub 默认紫）。
const labelColor = "5319e7"

// doBody 与 client.go 的 do 同骨架（App 401 作废重取一次），但支持 JSON
// 请求体——issue 写操作（POST/PATCH 带 body）需要。为不动 client.go，
// 认证/重试骨架在此复制并注明。
func (c *Client) doBody(ctx context.Context, repo, method, path string, in, out any) error {
	token, terr := c.auth(ctx, repo)
	if terr != nil {
		return fmt.Errorf("取认证 token: %w", terr)
	}
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+path, body)
		if err != nil {
			return err
		}
		c.setHeaders(req, token)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
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
			c.app.Invalidate(repo)
			if nt, nerr := c.auth(ctx, repo); nerr == nil {
				token = nt
				continue
			}
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
		}
		if out == nil || len(raw) == 0 {
			return nil
		}
		return json.Unmarshal(raw, out)
	}
}

// ListOpenIssuesByLabel 列出同时带全部指定标签的 open issue（§3.10.1：
// 同键 24h 复用 issue 的查找入口——按 ciorch + 场景标签找）。
func (c *Client) ListOpenIssuesByLabel(ctx context.Context, repo string, labels []string) ([]Issue, error) {
	q := url.Values{}
	q.Set("labels", joinComma(labels))
	q.Set("state", "open")
	q.Set("per_page", "100")
	var out []Issue
	// issues 列表少（事故量级个位数），单页足够；超出翻页由 per_page=100 兜底
	if err := c.doBody(ctx, repo, http.MethodGet, fmt.Sprintf("repos/%s/issues?%s", repo, q.Encode()), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetIssue 取单个 issue（按号；校验 state/标题用）。
func (c *Client) GetIssue(ctx context.Context, repo string, number int64) (Issue, error) {
	var out Issue
	if err := c.doBody(ctx, repo, http.MethodGet, fmt.Sprintf("repos/%s/issues/%d", repo, number), nil, &out); err != nil {
		return Issue{}, err
	}
	return out, nil
}

// CreateIssue 开 issue（标题/正文/标签；标签须先经 EnsureLabels 存在）。
func (c *Client) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (Issue, error) {
	var out Issue
	in := struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}{title, body, labels}
	if err := c.doBody(ctx, repo, http.MethodPost, fmt.Sprintf("repos/%s/issues", repo), in, &out); err != nil {
		return Issue{}, fmt.Errorf("开 issue: %w", err)
	}
	return out, nil
}

// EnsureLabels 确认标签存在，缺失则创建（§3.10.2"标签不存在时先创建"）。
func (c *Client) EnsureLabels(ctx context.Context, repo string, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	var existing []struct {
		Name string `json:"name"`
	}
	if err := c.doBody(ctx, repo, http.MethodGet, fmt.Sprintf("repos/%s/labels?per_page=100", repo), nil, &existing); err != nil {
		return fmt.Errorf("列标签: %w", err)
	}
	have := map[string]bool{}
	for _, l := range existing {
		have[l.Name] = true
	}
	for _, name := range labels {
		if have[name] {
			continue
		}
		in := struct {
			Name  string `json:"name"`
			Color string `json:"color"`
		}{name, labelColor}
		if err := c.doBody(ctx, repo, http.MethodPost, fmt.Sprintf("repos/%s/labels", repo), in, nil); err != nil {
			// 已存在（422 already_exists）视为成功——并发补建/人工已建
			if strings.Contains(err.Error(), "422") {
				continue
			}
			return fmt.Errorf("建标签 %s: %w", name, err)
		}
	}
	return nil
}

// AddIssueLabels 给 issue 追加标签（needs-human 时用）。
func (c *Client) AddIssueLabels(ctx context.Context, repo string, number int64, labels []string) error {
	in := struct {
		Labels []string `json:"labels"`
	}{labels}
	return c.doBody(ctx, repo, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/labels", repo, number), in, nil)
}

// AddIssueComment 追加评论（60K 截断由 incident 模块做，这里原样发）。
func (c *Client) AddIssueComment(ctx context.Context, repo string, number int64, body string) error {
	in := struct {
		Body string `json:"body"`
	}{body}
	return c.doBody(ctx, repo, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", repo, number), in, nil)
}

// CloseIssue 关闭 issue（PATCH state=closed）。
func (c *Client) CloseIssue(ctx context.Context, repo string, number int64) error {
	return c.setIssueState(ctx, repo, number, "closed")
}

// ReopenIssue 重开 issue（同键 24h 内复用：resolved 后再次发生时 reopen）。
func (c *Client) ReopenIssue(ctx context.Context, repo string, number int64) error {
	return c.setIssueState(ctx, repo, number, "open")
}

func (c *Client) setIssueState(ctx context.Context, repo string, number int64, state string) error {
	in := struct {
		State string `json:"state"`
	}{state}
	return c.doBody(ctx, repo, http.MethodPatch, fmt.Sprintf("repos/%s/issues/%d", repo, number), in, nil)
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
