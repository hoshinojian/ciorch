package githubapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 分页：Link rel="next" 两页合并；请求头带认证与 API 版本。
func TestListRunsPaginatesLinkHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer TESTTOKEN123456" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			w.WriteHeader(400)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/repos/a/b/actions/runs") {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("page") == "" || r.URL.Query().Get("page") == "1" {
			w.Header().Set("Link", `<`+srvBase(r)+`/repos/a/b/actions/runs?per_page=100&page=2>; rel="next"`)
			w.Write([]byte(`{"total_count":3,"workflow_runs":[{"id":1,"created_at":"2026-11-18T00:00:00Z"},{"id":2,"created_at":"2026-11-19T00:00:00Z"}]}`))
			return
		}
		w.Write([]byte(`{"total_count":3,"workflow_runs":[{"id":3,"created_at":"2026-11-20T00:00:00Z"}]}`))
	}))
	defer srv.Close()

	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL))
	since := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	runs, err := c.ListRuns(context.Background(), "a/b", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[2].ID != 3 {
		t.Fatalf("两页应合并 3 个 run: %+v", runs)
	}
}

// since 过滤参数按 RFC3339 传给 API（GitHub created=>= 语法）。
func TestListRunsSinceParam(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
	}))
	defer srv.Close()

	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL))
	since := time.Date(2026, 11, 13, 8, 30, 0, 0, time.UTC)
	if _, err := c.ListRuns(context.Background(), "a/b", since); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "created=%3E%3D2026-11-13T08%3A30%3A00Z") {
		t.Fatalf("since 应以 created>=RFC3339 传递，got %q", gotQuery)
	}
	if !strings.Contains(gotQuery, "per_page=100") {
		t.Fatalf("应 per_page=100，got %q", gotQuery)
	}
}

// jobs 解析：queued（无 started_at）与 in_progress/completed 都可表达。
func TestListJobsParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/a/b/actions/runs/42/jobs") {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("filter") != "latest" {
			w.WriteHeader(400)
			return
		}
		w.Write([]byte(`{"jobs":[
			{"id":1,"name":"test","status":"in_progress","created_at":"2026-11-20T01:00:00Z","started_at":"2026-11-20T01:03:00Z","runner_name":"cirunner-team2-ci-sgp1-01"},
			{"id":2,"name":"lint","status":"queued","created_at":"2026-11-20T01:00:00Z"}
		]}`))
	}))
	defer srv.Close()

	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL))
	jobs, err := c.ListJobs(context.Background(), "a/b", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("应 2 个 job: %+v", jobs)
	}
	if jobs[0].RunnerName != "cirunner-team2-ci-sgp1-01" || jobs[0].StartedAt.IsZero() {
		t.Fatalf("in_progress job 解析不对: %+v", jobs[0])
	}
	if !jobs[1].StartedAt.IsZero() {
		t.Fatalf("queued job 不应有 started_at: %+v", jobs[1])
	}
}

// API 错误透传为带状态码与 URL 的错误。
func TestAPIErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"message":"rate limit"}`))
	}))
	defer srv.Close()

	c := NewClient("TESTTOKEN123456", WithBaseURL(srv.URL))
	_, err := c.ListJobs(context.Background(), "a/b", 1)
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("应带状态码报错，got %v", err)
	}
}

func srvBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
