// Package llm 是 DeepSeek（OpenAI 兼容）chat/completions 客户端
// （REDESIGN §3.10.4）：JSON 模式、temperature=0、max_tokens=2000、超时 90s、
// 5xx 重试 1 次。系统提示词见 prompt.go。
//
// 调用限额（每事故 3 次 / 每日 20 次）不在本客户端——由 incident 模块执行
// （它才知道事故边界与跨日计数）；本包只管一次调用的协议正确性。
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client 是 chat/completions 客户端（并发安全：无共享可变状态）。
type Client struct {
	BaseURL string // 如 https://api.deepseek.com（不带 /chat/completions）
	Model   string // 如 deepseek-chat
	APIKey  string
	// HTTP 可注入测试服务器；默认 90s 超时（§3.10.4）。
	HTTP *http.Client
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type completeRequest struct {
	Model          string         `json:"model"`
	Messages       []message      `json:"messages"`
	Temperature    float64        `json:"temperature"`
	ResponseFormat responseFormat `json:"response_format"`
	MaxTokens      int            `json:"max_tokens"`
}

type completeResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete 发起一次诊断调用：system 是系统提示词（prompt.go 构建），
// user 是诊断材料 JSON。返回模型输出（应为 JSON 文本，解析由调用方做）。
// 5xx/网络错误重试 1 次（0.5s 退避）；4xx 不重试。
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	body, err := json.Marshal(completeRequest{
		Model:          c.Model,
		Messages:       []message{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Temperature:    0,
		ResponseFormat: responseFormat{Type: "json_object"},
		MaxTokens:      2000,
	})
	if err != nil {
		return "", err
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 90 * time.Second}
	}
	url := c.BaseURL + "/chat/completions"
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("llm 调用: %w", err)
			continue // 网络错误重试 1 次
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("llm %d: %s", resp.StatusCode, truncate(raw, 200))
			continue // 5xx 重试 1 次
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("llm %d（不重试）: %s", resp.StatusCode, truncate(raw, 200))
		}
		var out completeResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", fmt.Errorf("llm 响应解析失败: %v（body: %s）", err, truncate(raw, 200))
		}
		if out.Error != nil {
			return "", fmt.Errorf("llm 错误: %s", out.Error.Message)
		}
		if len(out.Choices) == 0 {
			return "", fmt.Errorf("llm 响应无 choices: %s", truncate(raw, 200))
		}
		return out.Choices[0].Message.Content, nil
	}
	return "", lastErr
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
