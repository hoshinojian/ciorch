// Package server 是 ciorch 的 HTTP 面（REDESIGN §3.11）：/webhook（GitHub
// App 事件入口）、/healthz（无鉴权存活探针）、/status（Bearer token）。
// 只监听 127.0.0.1，公网由 Caddy 反代终结 TLS。
package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hoshinojian/ciorch/internal/observe"
)

// Deps 是 server 依赖（测试可注入）。
type Deps struct {
	// WebhookSecret 验签密钥（X-Hub-Signature-256）。
	WebhookSecret []byte
	// Repos 是白名单（owner/name → true）：不在名单的仓直接 200 丢弃。
	Repos map[string]bool
	// Cache 是 observe 的 webhook job 缓存（写入 workflow_job 快照）。
	Cache *observe.JobCache
	// Trigger 请求某仓立即 reconcile（2s 防抖由 daemon.Trigger 合并）。
	Trigger func(repo string)
	// OnIssueClosed 带标签 issue 关闭通知（阶段 5 接事故模块；nil=只记日志）。
	OnIssueClosed func(repo string, number int)
	// LastOK 返回最近一次全仓 reconcile 成功时刻（/healthz 判据）。
	LastOK func() time.Time
	// PollIdle 是空闲轮询间隔（healthz 阈值 = 3×PollIdle）。
	PollIdle time.Duration
	// StatusBody 返回 /status 的 JSON 载荷。
	StatusBody func() any
	// StatusToken 是 /status 的 Bearer token。
	StatusToken string
	Log         *log.Logger
}

// maxWebhookBody 是 webhook body 上限（§3.4.5：5MB）。
const maxWebhookBody = 5 << 20

// New 构造 HTTP handler。
func New(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", d.handleWebhook)
	mux.HandleFunc("/healthz", d.handleHealthz)
	mux.HandleFunc("/status", d.handleStatus)
	return mux
}

// handleWebhook：验签 → 按 X-GitHub-Event 分发。返回要快（<1s）：只入
// 缓存与触发 reconcile，不在请求内做观察/决策。
func (d *Deps) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > maxWebhookBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !d.verifySignature(r, body) {
		// 验签不过 401 且不解析 body（§3.4.5）
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	switch event {
	case "ping":
		w.WriteHeader(http.StatusOK)
	case "workflow_job":
		d.handleWorkflowJob(w, body)
	case "issues":
		d.handleIssues(w, body)
	default:
		// 其他事件 200 忽略
		w.WriteHeader(http.StatusOK)
	}
}

// verifySignature：X-Hub-Signature-256: sha256=<hex>，hmac.Equal 常量时间比较。
func (d *Deps) verifySignature(r *http.Request, body []byte) bool {
	sigHeader := r.Header.Get("X-Hub-Signature-256")
	const prefix = "sha256="
	if !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	got, err := hex.DecodeString(sigHeader[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, d.WebhookSecret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}

type wfJobEvent struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	WorkflowJob struct {
		ID         int64     `json:"id"`
		RunID      int64     `json:"run_id"`
		Status     string    `json:"status"`
		Labels     []string  `json:"labels"`
		CreatedAt  time.Time `json:"created_at"`
		StartedAt  time.Time `json:"started_at"`
		RunnerName string    `json:"runner_name"`
	} `json:"workflow_job"`
}

func (d *Deps) handleWorkflowJob(w http.ResponseWriter, body []byte) {
	var ev wfJobEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	repo := ev.Repository.FullName
	if !d.Repos[repo] {
		// 不在白名单 → 200 丢弃（§3.4.5）
		w.WriteHeader(http.StatusOK)
		return
	}
	if d.Cache != nil {
		d.Cache.Put(observe.JobCacheEntry{
			Job: observe.Job{
				ID: ev.WorkflowJob.ID, RunID: ev.WorkflowJob.RunID,
				Status: ev.WorkflowJob.Status, Labels: ev.WorkflowJob.Labels,
				CreatedAt: ev.WorkflowJob.CreatedAt, StartedAt: ev.WorkflowJob.StartedAt,
				RunnerName: ev.WorkflowJob.RunnerName,
			},
			UpdatedAt: time.Now(),
		})
	}
	if d.Trigger != nil {
		d.Trigger(repo)
	}
	w.WriteHeader(http.StatusOK)
}

type issuesEvent struct {
	Action     string `json:"action"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue struct {
		Number int `json:"number"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"issue"`
}

func (d *Deps) handleIssues(w http.ResponseWriter, body []byte) {
	var ev issuesEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if ev.Action != "closed" {
		w.WriteHeader(http.StatusOK)
		return
	}
	hasCiorch := false
	for _, l := range ev.Issue.Labels {
		if l.Name == "ciorch" {
			hasCiorch = true
		}
	}
	if !hasCiorch {
		w.WriteHeader(http.StatusOK)
		return
	}
	// 人工关闭带 ciorch 标签的 issue：阶段 5 接事故模块（清熔断/解除 pause）
	if d.OnIssueClosed != nil {
		d.OnIssueClosed(ev.Repository.FullName, ev.Issue.Number)
	} else if d.Log != nil {
		d.Log.Printf("[webhook] issues.closed repo=%s #%d（带 ciorch 标签；阶段 5 接事故模块）",
			ev.Repository.FullName, ev.Issue.Number)
	}
	w.WriteHeader(http.StatusOK)
}

// handleHealthz：最近一次全仓 reconcile 成功距今 < 3×PollIdle → 200 ok；
// 否则 503 + 原因（不泄露细节）。
func (d *Deps) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if d.LastOK == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	last := d.LastOK()
	if last.IsZero() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	if time.Since(last) > 3*d.PollIdle {
		http.Error(w, "stale", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

// handleStatus：Bearer token 鉴权后返回 StatusBody JSON。
func (d *Deps) handleStatus(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if auth != "Bearer "+d.StatusToken || d.StatusToken == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body := d.StatusBody()
	if body == nil {
		body = map[string]any{"status": "ok"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}
