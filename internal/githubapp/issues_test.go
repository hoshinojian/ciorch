package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGH 是最小 GitHub 事故 API 假服务器（只在内存里记账）。
type fakeGH struct {
	mu        chan struct{} // 简单互斥（测试单协程，实际不用锁）
	issues    map[int64]*fakeIssue
	nextNum   int64
	labels    map[string]bool
	comments  map[int64][]string
	calls     []string
	logBytes  []byte
	rerunRuns []int64
	failLogOn bool
}

type fakeIssue struct {
	number int64
	title  string
	body   string
	state  string
	labels []string
}

func newFakeGH() *fakeGH {
	return &fakeGH{issues: map[int64]*fakeIssue{}, labels: map[string]bool{}, comments: map[int64][]string{}}
}

func (f *fakeGH) handler(w http.ResponseWriter, r *http.Request) {
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels"):
		var out []map[string]any
		for name := range f.labels {
			out = append(out, map[string]any{"name": name, "color": "5319e7"})
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/labels":
		var in struct {
			Name string `json:"name"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if f.labels[in.Name] {
			w.WriteHeader(422) // already_exists
			return
		}
		f.labels[in.Name] = true
		w.WriteHeader(201)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues") && r.URL.Query().Get("labels") != "":
		want := strings.Split(r.URL.Query().Get("labels"), ",")
		var out []map[string]any
		for _, is := range f.issues {
			if is.state != "open" {
				continue
			}
			ok := true
			for _, wl := range want {
				found := false
				for _, l := range is.labels {
					if l == wl {
						found = true
					}
				}
				if !found {
					ok = false
				}
			}
			if ok {
				out = append(out, map[string]any{"number": is.number, "title": is.title, "state": is.state})
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
		var in struct {
			Title, Body string `json:",omitempty"`
		}
		var full struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		json.NewDecoder(r.Body).Decode(&full)
		_ = in
		f.nextNum++
		is := &fakeIssue{number: f.nextNum, title: full.Title, body: full.Body, state: "open", labels: full.Labels}
		f.issues[is.number] = is
		json.NewEncoder(w).Encode(map[string]any{"number": is.number, "title": is.title, "state": "open", "html_url": fmt.Sprintf("https://gh/o/r/issues/%d", is.number)})
	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/issues/"):
		var in struct {
			State string `json:"state"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		num := int64(0)
		fmt.Sscanf(r.URL.Path, "/repos/o/r/issues/%d", &num)
		f.issues[num].state = in.State
		w.WriteHeader(200)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
		var in struct {
			Body string `json:"body"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		num := int64(0)
		fmt.Sscanf(strings.TrimSuffix(r.URL.Path, "/comments"), "/repos/o/r/issues/%d", &num)
		f.comments[num] = append(f.comments[num], in.Body)
		w.WriteHeader(201)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/labels") && strings.Contains(r.URL.Path, "/issues/"):
		var in struct {
			Labels []string `json:"labels"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		num := int64(0)
		fmt.Sscanf(r.URL.Path, "/repos/o/r/issues/%d/labels", &num)
		for _, l := range in.Labels {
			f.issues[num].labels = append(f.issues[num].labels, l)
		}
		w.WriteHeader(200)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
		if f.failLogOn {
			w.WriteHeader(404)
			return
		}
		http.Redirect(w, r, "/download/log.txt", http.StatusFound)
	case r.Method == http.MethodGet && r.URL.Path == "/download/log.txt":
		w.Write(f.logBytes)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/rerun-failed-jobs"):
		var runID int64
		fmt.Sscanf(r.URL.Path, "/repos/o/r/actions/runs/%d/rerun-failed-jobs", &runID)
		f.rerunRuns = append(f.rerunRuns, runID)
		w.WriteHeader(201)
	default:
		w.WriteHeader(404)
	}
}

func newIssueClient(t *testing.T, f *fakeGH) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return NewClient("test-token", WithBaseURL(srv.URL))
}

// 开 issue：路径/方法/请求体（标题、正文、标签）。
func TestCreateIssue(t *testing.T) {
	f := newFakeGH()
	c := newIssueClient(t, f)
	if err := c.EnsureLabels(context.Background(), "o/r", []string{"ciorch", "ciorch:a"}); err != nil {
		t.Fatal(err)
	}
	is, err := c.CreateIssue(context.Background(), "o/r", "[ciorch] runner 假活：m1（o/r）", "正文", []string{"ciorch", "ciorch:a"})
	if err != nil {
		t.Fatal(err)
	}
	if is.Number != 1 || is.State != "open" {
		t.Fatalf("CreateIssue 返回：%+v", is)
	}
	got := f.issues[1]
	if got.title != "[ciorch] runner 假活：m1（o/r）" || got.body != "正文" || len(got.labels) != 2 {
		t.Fatalf("issue 内容：%+v", got)
	}
	if !f.labels["ciorch"] || !f.labels["ciorch:a"] {
		t.Fatalf("标签应已补建：%v", f.labels)
	}
}

// EnsureLabels：已存在的跳过（不发 POST）；422 already_exists 容忍。
func TestEnsureLabelsIdempotent(t *testing.T) {
	f := newFakeGH()
	f.labels["ciorch"] = true
	c := newIssueClient(t, f)
	if err := c.EnsureLabels(context.Background(), "o/r", []string{"ciorch", "ciorch:b"}); err != nil {
		t.Fatal(err)
	}
	posts := 0
	for _, call := range f.calls {
		if call == "POST /repos/o/r/labels" {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("只应补建缺失的一个标签，POST %d 次", posts)
	}
}

// 按标签列 open issue（同键复用的查找）。
func TestListOpenIssuesByLabel(t *testing.T) {
	f := newFakeGH()
	f.nextNum = 2
	f.issues[1] = &fakeIssue{number: 1, state: "open", labels: []string{"ciorch", "ciorch:a"}}
	f.issues[2] = &fakeIssue{number: 2, state: "open", labels: []string{"ciorch", "ciorch:b"}}
	f.issues[3] = &fakeIssue{number: 3, state: "closed", labels: []string{"ciorch", "ciorch:a"}}
	c := newIssueClient(t, f)
	got, err := c.ListOpenIssuesByLabel(context.Background(), "o/r", []string{"ciorch", "ciorch:a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("应只列出 1 号 open issue：%+v", got)
	}
}

// 评论/关闭/重开。
func TestCommentCloseReopen(t *testing.T) {
	f := newFakeGH()
	f.issues[7] = &fakeIssue{number: 7, state: "open"}
	c := newIssueClient(t, f)
	if err := c.AddIssueComment(context.Background(), "o/r", 7, "诊断结论：xxx"); err != nil {
		t.Fatal(err)
	}
	if err := c.AddIssueLabels(context.Background(), "o/r", 7, []string{"needs-human"}); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseIssue(context.Background(), "o/r", 7); err != nil {
		t.Fatal(err)
	}
	if f.issues[7].state != "closed" {
		t.Fatal("应已关闭")
	}
	if len(f.comments[7]) != 1 || f.comments[7][0] != "诊断结论：xxx" {
		t.Fatalf("评论：%v", f.comments[7])
	}
	if err := c.ReopenIssue(context.Background(), "o/r", 7); err != nil {
		t.Fatal(err)
	}
	if f.issues[7].state != "open" {
		t.Fatal("应已重开")
	}
}

// job 日志：302 跳转后拿到原文；404 报错。
func TestGetJobLogRedirect(t *testing.T) {
	f := newFakeGH()
	f.logBytes = []byte(strings.Repeat("logline\n", 100))
	c := newIssueClient(t, f)
	raw, err := c.GetJobLog(context.Background(), "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != len("logline\n")*100 {
		t.Fatalf("日志长度：%d", len(raw))
	}
	f.failLogOn = true
	if _, err := c.GetJobLog(context.Background(), "o/r", 42); err == nil {
		t.Fatal("404 应报错")
	}
}

// rerun-failed-jobs。
func TestRerunFailedJobs(t *testing.T) {
	f := newFakeGH()
	c := newIssueClient(t, f)
	if err := c.RerunFailedJobs(context.Background(), "o/r", 12345); err != nil {
		t.Fatal(err)
	}
	if len(f.rerunRuns) != 1 || f.rerunRuns[0] != 12345 {
		t.Fatalf("rerun 调用：%v", f.rerunRuns)
	}
}
