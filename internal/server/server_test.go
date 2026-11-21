package server

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/observe"
)

func deps(secret string) Deps {
	return Deps{
		WebhookSecret: []byte(secret),
		Repos:         map[string]bool{"hoshinojian/nms": true},
		Cache:         observe.NewJobCache(),
		Trigger:       func(repo string) {},
		PollIdle:      60 * time.Second,
		StatusToken:   "STATUSTOKEN123456",
	}
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func postWebhook(t *testing.T, h http.Handler, event string, secret string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signBody(secret, body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// §8.1 webhook：签名正确/错误/缺失。
func TestWebhookSignature(t *testing.T) {
	d := deps("topsecret")
	h := New(d)
	body := []byte(`{"action":"queued","repository":{"full_name":"hoshinojian/nms"},"workflow_job":{"id":1}}`)

	if rec := postWebhook(t, h, "workflow_job", "topsecret", body); rec.Code != 200 {
		t.Fatalf("正确签名应 200: %d", rec.Code)
	}
	// 错误签名 → 401
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-Hub-Signature-256", signBody("wrong", body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("错误签名应 401: %d", rec.Code)
	}
	// 缺失签名 → 401
	req2 := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req2.Header.Set("X-GitHub-Event", "workflow_job")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 401 {
		t.Fatalf("缺失签名应 401: %d", rec2.Code)
	}
}

// 非白名单仓丢弃；白名单仓写缓存并触发。
func TestWebhookWhitelistAndCache(t *testing.T) {
	var triggered []string
	d := deps("s")
	d.Trigger = func(repo string) { triggered = append(triggered, repo) }
	h := New(d)

	// 非白名单
	rec := postWebhook(t, h, "workflow_job", "s",
		[]byte(`{"repository":{"full_name":"someone/else"},"workflow_job":{"id":9,"status":"queued"}}`))
	if rec.Code != 200 {
		t.Fatalf("非白名单应 200 丢弃: %d", rec.Code)
	}
	if len(triggered) != 0 {
		t.Fatal("非白名单不得触发")
	}

	// 白名单：缓存 + 触发
	rec = postWebhook(t, h, "workflow_job", "s",
		[]byte(`{"repository":{"full_name":"hoshinojian/nms"},"workflow_job":{"id":42,"run_id":7,"status":"queued","labels":["self-hosted","ci"],"created_at":"2026-11-21T12:00:00Z"}}`))
	if rec.Code != 200 {
		t.Fatalf("白名单应 200: %d", rec.Code)
	}
	if len(triggered) != 1 || triggered[0] != "hoshinojian/nms" {
		t.Fatalf("应触发该仓: %v", triggered)
	}
	snap := d.Cache.Snapshot(time.Now())
	if len(snap) != 1 || snap[0].Job.ID != 42 || snap[0].Job.Status != "queued" {
		t.Fatalf("缓存不对: %+v", snap)
	}
}

// body 超限 → 413。
func TestWebhookBodyLimit(t *testing.T) {
	d := deps("s")
	h := New(d)
	big := bytes.Repeat([]byte("a"), (5<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(big))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", signBody("s", big))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应 413: %d", rec.Code)
	}
}

// ping → 200；issues.closed 带 ciorch 标签 → 回调；其他 action 忽略。
func TestWebhookPingAndIssues(t *testing.T) {
	d := deps("s")
	closed := map[string]int{}
	d.OnIssueClosed = func(repo string, n int) { closed[repo] = n }
	h := New(d)

	if rec := postWebhook(t, h, "ping", "s", []byte(`{}`)); rec.Code != 200 {
		t.Fatal("ping 应 200")
	}
	// 非 closed action
	if rec := postWebhook(t, h, "issues", "s",
		[]byte(`{"action":"opened","repository":{"full_name":"hoshinojian/nms"},"issue":{"number":5,"labels":[{"name":"ciorch"}]}}`)); rec.Code != 200 {
		t.Fatal("opened 应忽略")
	}
	if len(closed) != 0 {
		t.Fatal("opened 不回调")
	}
	// closed + ciorch 标签
	if rec := postWebhook(t, h, "issues", "s",
		[]byte(`{"action":"closed","repository":{"full_name":"hoshinojian/nms"},"issue":{"number":5,"labels":[{"name":"ciorch"}]}}`)); rec.Code != 200 {
		t.Fatal("closed 应 200")
	}
	if closed["hoshinojian/nms"] != 5 {
		t.Fatalf("应回调: %v", closed)
	}
	// closed 无标签 → 忽略
	postWebhook(t, h, "issues", "s",
		[]byte(`{"action":"closed","repository":{"full_name":"hoshinojian/nms"},"issue":{"number":6,"labels":[]}}`))
	if len(closed) != 1 {
		t.Fatalf("无标签不回调: %v", closed)
	}
}

// healthz：新鲜 200；过期 503；未就绪 503。
func TestHealthz(t *testing.T) {
	// New 按值收 Deps：先设字段再构造
	mk := func(lastOK func() time.Time) http.Handler {
		d := deps("s")
		d.LastOK = lastOK
		return New(d)
	}

	if rec := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mk(func() time.Time { return time.Time{} }).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec
	}(); rec.Code != 503 {
		t.Fatalf("未就绪应 503: %d", rec.Code)
	}

	rec := httptest.NewRecorder()
	mk(func() time.Time { return time.Now().Add(-time.Minute) }).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("新鲜应 200: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mk(func() time.Time { return time.Now().Add(-4 * time.Minute) }).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil)) // >3×60s
	if rec.Code != 503 {
		t.Fatalf("过期应 503: %d", rec.Code)
	}
}

// status：无 token 401；正确 token 返回 JSON。
func TestStatusAuth(t *testing.T) {
	d := deps("s")
	d.StatusBody = func() any { return map[string]int{"q": 3} }
	h := New(d)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != 401 {
		t.Fatalf("无 token 应 401: %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer STATUSTOKEN123456")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("正确 token 应 200: %d", rec.Code)
	}
	var m map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m["q"] != 3 {
		t.Fatalf("载荷不对: %v", rec.Body.String())
	}
}
