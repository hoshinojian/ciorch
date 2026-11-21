// 诊断材料相关的 GitHub API（REDESIGN §3.10.3/§3.10.5）：job 日志下载、
// 重跑失败 job。同样只新增方法，不改 client.go。
package githubapp

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// GetJobLog 取 job 日志原文（GET /actions/jobs/{id}/logs 返回 302 跳转下载，
// 客户端自动跟随；跨域跳转时 Authorization 会被 Go 标准库剥掉——签名 URL
// 本就无需认证）。上限 2MB，尾部截取（32KB tail）由 incident 模块做。
func (c *Client) GetJobLog(ctx context.Context, repo string, jobID int64) ([]byte, error) {
	token, terr := c.auth(ctx, repo)
	if terr != nil {
		return nil, fmt.Errorf("取认证 token: %w", terr)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/actions/jobs/%d/logs", c.baseURL, repo, jobID), nil)
	if err != nil {
		return nil, err
	}
	c.setHeaders(req, token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("取 job %d 日志: %w", jobID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("取 job %d 日志: HTTP %d", jobID, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
}

// RerunFailedJobs 重跑 run 的失败 job（§3.10.5 白名单动作 rerun_failed_jobs）。
func (c *Client) RerunFailedJobs(ctx context.Context, repo string, runID int64) error {
	return c.doBody(ctx, repo, http.MethodPost, fmt.Sprintf("repos/%s/actions/runs/%d/rerun-failed-jobs", repo, runID), nil, nil)
}
