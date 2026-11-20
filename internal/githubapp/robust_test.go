package githubapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func zeroSleep(c *Client) {
	c.sleep = func(ctx context.Context, d time.Duration) error { return nil }
}

// §3.4.3/§8.1：5xx 同轮退避重试 2 次（注入零等待）。
func TestRetryOn5xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	}))
	defer srv.Close()
	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL), WithRetryBackoff(0, 0))
	zeroSleep(c)
	if _, err := c.ListRuns(context.Background(), "a/b", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("两次 503 后第三次应成功: %v", err)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("应共调用 3 次，got %d", calls)
	}
}

// 4xx 不重试。
func TestNoRetryOn4xx(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(404)
	}))
	defer srv.Close()
	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL), WithRetryBackoff(0, 0))
	zeroSleep(c)
	if _, err := c.ListRuns(context.Background(), "a/b", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("404 应报错")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("4xx 不应重试，got %d 次", calls)
	}
}

// ETag 条件请求：304 直接用缓存 body（§8.1）。
func TestETag304UsesCache(t *testing.T) {
	var calls int32
	var sawConditional bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("ETag", `"abc"`)
			w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":7,"created_at":"2026-11-20T11:00:00Z"}]}`))
			return
		}
		if r.Header.Get("If-None-Match") == `"abc"` {
			sawConditional = true
			w.WriteHeader(304) // not modified
			return
		}
		w.Write([]byte(`{"total_count":9}`))
	}))
	defer srv.Close()
	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL))
	zeroSleep(c)
	runs, err := c.ListRuns(context.Background(), "a/b", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC))
	if err != nil || len(runs) != 1 {
		t.Fatalf("首次: runs=%v err=%v", runs, err)
	}
	runs2, err := c.ListRuns(context.Background(), "a/b", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC))
	if err != nil || len(runs2) != 1 || runs2[0].ID != 7 {
		t.Fatalf("304 应用缓存: runs=%v err=%v", runs2, err)
	}
	if !sawConditional {
		t.Fatal("第二次请求应带 If-None-Match")
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("应共 2 次请求，got %d", calls)
	}
}

// 429 带 Retry-After：遵守（注入 sleeper 记录等待）后重试成功。
func Test429RetryAfter(t *testing.T) {
	var calls int32
	var slept []time.Duration
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(`{"runners":[]}`))
	}))
	defer srv.Close()
	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL), WithRetryBackoff(0))
	c.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	if _, err := c.ListRunners(context.Background(), "a/b"); err != nil {
		t.Fatalf("429 后应重试成功: %v", err)
	}
	if len(slept) == 0 || slept[0] != 2*time.Second {
		t.Fatalf("应遵守 Retry-After=2s，slept=%v", slept)
	}
}

// RateRemaining 暴露给 daemon 做 <10% 时轮询 ×4。
func TestRateRemainingExposed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "42")
		w.Write([]byte(`{"runners":[]}`))
	}))
	defer srv.Close()
	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL))
	if _, err := c.ListRunners(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
	if c.RateRemaining() != 42 {
		t.Fatalf("应读到 42，got %d", c.RateRemaining())
	}
}
