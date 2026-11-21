package incident

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/githubapp"
)

var t0 = time.Date(2026, 11, 21, 14, 0, 0, 0, time.UTC)

// ---------- fakes ----------

type fakeIssues struct {
	mu       sync.Mutex
	nextNum  int64
	created  []createdIssue
	comments map[int64][]string
	addedLbl map[int64][]string
	closed   []int64
	reopened []int64
	ensured  [][]string
}

type createdIssue struct {
	repo, title, body string
	labels            []string
	number            int64
}

func (f *fakeIssues) EnsureLabels(ctx context.Context, repo string, labels []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensured = append(f.ensured, labels)
	return nil
}

func (f *fakeIssues) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (githubapp.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextNum++
	f.created = append(f.created, createdIssue{repo, title, body, labels, f.nextNum})
	return githubapp.Issue{Number: f.nextNum, State: "open"}, nil
}

func (f *fakeIssues) AddIssueComment(ctx context.Context, repo string, number int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.comments == nil {
		f.comments = map[int64][]string{}
	}
	f.comments[number] = append(f.comments[number], body)
	return nil
}

func (f *fakeIssues) AddIssueLabels(ctx context.Context, repo string, number int64, labels []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addedLbl == nil {
		f.addedLbl = map[int64][]string{}
	}
	f.addedLbl[number] = append(f.addedLbl[number], labels...)
	return nil
}

func (f *fakeIssues) CloseIssue(ctx context.Context, repo string, number int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, number)
	return nil
}

func (f *fakeIssues) ReopenIssue(ctx context.Context, repo string, number int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reopened = append(f.reopened, number)
	return nil
}

type fakeExec struct {
	mu       sync.Mutex
	cleared  []string
	restarts []string
	drains   []string
	replaces []string
	pauses   map[string]time.Duration
	breakers []string
	switches []string
	reruns   []int64
}

func (f *fakeExec) HumanCleared(repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, repo)
}

func (f *fakeExec) RestartRunner(ctx context.Context, repo, machine string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, machine)
	return nil
}

func (f *fakeExec) DrainMachine(ctx context.Context, repo, machine string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains = append(f.drains, machine)
	return nil
}

func (f *fakeExec) ReplaceMachine(ctx context.Context, repo, machine string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaces = append(f.replaces, machine)
	return nil
}

func (f *fakeExec) PauseRepo(repo string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauses == nil {
		f.pauses = map[string]time.Duration{}
	}
	f.pauses[repo] = d
}

func (f *fakeExec) ClearBreaker(repo string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.breakers = append(f.breakers, repo)
}

func (f *fakeExec) SwitchPool(repo, pool string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.switches = append(f.switches, repo+"→"+pool)
}

func (f *fakeExec) RerunFailedJobs(ctx context.Context, repo string, runID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reruns = append(f.reruns, runID)
	return nil
}

// testClock 是可推进的注入时钟。
type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time { return c.t }

func newMgr(t *testing.T, issues *fakeIssues, exec *fakeExec) (*Manager, *testClock) {
	t.Helper()
	m := New(issues, exec, Options{MaintainerLogin: "maintainer"}, t.TempDir(), nil)
	m.Log.SetOutput(os.NewFile(0, os.DevNull))
	clock := &testClock{t: t0}
	m.Now = clock.Now
	return m, clock
}

func actA(subject, reason string) decide.Action {
	return decide.Action{Kind: decide.ActIncident, IncidentType: "A", IncidentKey: subject, Reason: reason}
}

// ---------- §8.1 incident：状态机与去重 ----------

// 去重（shadow 教训：场景 H 每轮重发）：同键连续 5 轮只开 1 个 issue。
func TestHandleDedupesSameKey(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	for i := 0; i < 5; i++ {
		if err := m.Handle(context.Background(), "o/r", actA("m1", "假活")); err != nil {
			t.Fatal(err)
		}
	}
	if len(issues.created) != 1 {
		t.Fatalf("同键去重应只开 1 个 issue，got %d", len(issues.created))
	}
	inc := m.incidents[Key{"o/r", "A", "m1"}]
	if inc == nil || inc.State != StateDiagnosing {
		t.Fatalf("新事故应到 diagnosing：%+v", inc)
	}
}

// 不同键不同 issue。
func TestDifferentKeysSeparateIssues(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	_ = m.Handle(context.Background(), "o/r", actA("m1", "r"))
	_ = m.Handle(context.Background(), "o/r", actA("m2", "r"))
	if len(issues.created) != 2 {
		t.Fatalf("不同主体应各开一个：%d", len(issues.created))
	}
}

// 恢复：判据稳定 2 轮 → resolved + 评论依据 + 关闭。
func TestRecoveryAfterQuietRounds(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	_ = m.Handle(context.Background(), "o/r", actA("m1", "假活"))
	m.Tick(context.Background(), "o/r") // quiet 1：不恢复
	if inc := m.incidents[Key{"o/r", "A", "m1"}]; inc.State == StateResolved {
		t.Fatal("1 轮安静不应恢复")
	}
	m.Tick(context.Background(), "o/r") // quiet 2 → resolved
	inc := m.incidents[Key{"o/r", "A", "m1"}]
	if inc.State != StateResolved {
		t.Fatalf("2 轮安静应 resolved：%s", inc.State)
	}
	if len(issues.closed) != 1 || issues.closed[0] != 1 {
		t.Fatalf("应关闭 issue #1：%v", issues.closed)
	}
	if len(issues.comments[1]) == 0 || !strings.Contains(issues.comments[1][0], "已恢复") {
		t.Fatalf("恢复评论：%v", issues.comments[1])
	}
}

// 复发清零安静计数。
func TestFireResetsQuiet(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	_ = m.Handle(context.Background(), "o/r", actA("m1", "r"))
	m.Tick(context.Background(), "o/r")
	_ = m.Handle(context.Background(), "o/r", actA("m1", "r")) // 复发：去重 + 清零
	m.Tick(context.Background(), "o/r")
	if inc := m.incidents[Key{"o/r", "A", "m1"}]; inc.State == StateResolved {
		t.Fatal("复发后不应立即恢复")
	}
}

// 同键 24h 内复用 issue（reopen，不新开）；超 24h 开新的。
func TestReuseIssueWithin24h(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, clock := newMgr(t, issues, exec)
	_ = m.Handle(context.Background(), "o/r", actA("m1", "r"))
	m.Tick(context.Background(), "o/r")
	m.Tick(context.Background(), "o/r") // resolved
	clock.t = t0.Add(10 * time.Hour)    // 24h 内复发
	_ = m.Handle(context.Background(), "o/r", actA("m1", "again"))
	if len(issues.created) != 1 || len(issues.reopened) != 1 || issues.reopened[0] != 1 {
		t.Fatalf("24h 内应复用（reopen #1）：%d created %v reopened", len(issues.created), issues.reopened)
	}
	if len(issues.comments[1]) != 2 || !strings.Contains(issues.comments[1][1], "再次出现") {
		t.Fatalf("复用应追加评论：%v", issues.comments[1])
	}
	m.Tick(context.Background(), "o/r")
	m.Tick(context.Background(), "o/r") // 再次恢复

	clock.t = t0.Add(40 * time.Hour) // 超 24h
	_ = m.Handle(context.Background(), "o/r", actA("m1", "third"))
	if len(issues.created) != 2 {
		t.Fatalf("超 24h 应新开 issue：%d", len(issues.created))
	}
}

// 人工关闭 issue → resolved + 清熔断/退避/pause 回调。
func TestHumanResolved(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	_ = m.Handle(context.Background(), "o/r", actA("m1", "r"))
	m.HumanResolved("o/r", 1)
	if inc := m.incidents[Key{"o/r", "A", "m1"}]; inc.State != StateResolved {
		t.Fatal("人工关闭应 resolved")
	}
	if len(exec.cleared) != 1 || exec.cleared[0] != "o/r" {
		t.Fatalf("应回调 HumanCleared：%v", exec.cleared)
	}
	// 无对应事故的 issue 号：不炸、不回调
	m.HumanResolved("o/r", 99)
	if len(exec.cleared) != 1 {
		t.Fatalf("无事故的 issue 不应回调：%v", exec.cleared)
	}
}

// issue 规范：标题 `[ciorch] <场景中文名>：<主体>（<repo>）`；标签 ciorch/ciorch:<a>；
// 正文四段模板。
func TestIssueFormat(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	m.Note("o/r", "action", "SSHAct restart-runner m1")
	if err := m.Handle(context.Background(), "o/r", actA("m1", "online 不接活")); err != nil {
		t.Fatal(err)
	}
	got := issues.created[0]
	if got.title != "[ciorch] runner 假活：m1（o/r）" {
		t.Fatalf("标题：%q", got.title)
	}
	if strings.Join(got.labels, ",") != "ciorch,ciorch:a" {
		t.Fatalf("标签：%v", got.labels)
	}
	for _, want := range []string{"## 发生了什么", "online 不接活", "## 时间线", "restart-runner", "## 固定逻辑已做的处置", "## 下一步"} {
		if !strings.Contains(got.body, want) {
			t.Errorf("正文应含 %q", want)
		}
	}
}

// 评论 60000 字符截断，保留末尾。
func TestCommentTruncation(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	_ = m.Handle(context.Background(), "o/r", actA("m1", "r"))
	inc := m.incidents[Key{"o/r", "A", "m1"}]
	long := strings.Repeat("前", 70000) + "TAIL-MARKER"
	m.comment(context.Background(), inc, long)
	got := issues.comments[1][0]
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Fatal("截断应保留末尾")
	}
	if !strings.Contains(got, "已截断") {
		t.Fatal("截断应有说明")
	}
	if n := len([]rune(got)); n > maxCommentChars+200 {
		t.Fatalf("截断后长度应 ~60000+说明，got %d", n)
	}
}

// dry-run（shadow）：不开 issue、不评论、不关闭——只日志与 jsonl。
func TestDryRunNoSideEffects(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	m.DryRun = true
	if err := m.Handle(context.Background(), "o/r", actA("m1", "r")); err != nil {
		t.Fatal(err)
	}
	m.Tick(context.Background(), "o/r")
	m.Tick(context.Background(), "o/r")
	if len(issues.created)+len(issues.comments)+len(issues.closed)+len(issues.reopened)+len(issues.ensured) != 0 {
		t.Fatalf("dry-run 必须零 issue 副作用：%+v", issues)
	}
	if len(exec.cleared) != 0 {
		t.Fatal("dry-run 不应触发人工清理回调（无人工介入）")
	}
	if inc := m.incidents[Key{"o/r", "A", "m1"}]; inc.State != StateResolved {
		t.Fatalf("dry-run 下状态机照常推进（可观察）：%s", inc.State)
	}
	// jsonl 仍记录（本地文件，无外部作用）
	raw, _ := os.ReadFile(m.jsonlPath)
	if !strings.Contains(string(raw), "\"event\":\"detected\"") || !strings.Contains(string(raw), "\"event\":\"resolved\"") {
		t.Fatalf("dry-run 应记 jsonl：%s", raw)
	}
}

// NoteRemedy：固定逻辑修好的 A/B/C 只写 jsonl 并计数，不开 issue；
// 计数出现在后续 issue 正文。
func TestNoteRemedyCountsOnly(t *testing.T) {
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, exec)
	m.NoteRemedy("o/r", "B", "m1", "冷启失败已替换")
	m.NoteRemedy("o/r", "B", "m1", "再次替换")
	if len(issues.created) != 0 {
		t.Fatal("单次修好的补救不开 issue")
	}
	raw, _ := os.ReadFile(m.jsonlPath)
	if strings.Count(string(raw), "\"event\":\"remedy\"") != 2 {
		t.Fatalf("jsonl 应有 2 条 remedy：%s", raw)
	}
	// 之后升级开 issue：正文带补救计数
	_ = m.Handle(context.Background(), "o/r", decide.Action{Kind: decide.ActIncident, IncidentType: "B", IncidentKey: "m1", Reason: "熔断"})
	if !strings.Contains(issues.created[0].body, "补救 2 次") {
		t.Fatalf("issue 正文应含补救计数：%q", issues.created[0].body)
	}
}

// Snapshot/Restore：事故表跨重启。
func TestSnapshotRestore(t *testing.T) {
	issues, _ := &fakeIssues{}, &fakeExec{}
	m, _ := newMgr(t, issues, &fakeExec{})
	_ = m.Handle(context.Background(), "o/r", actA("m1", "r"))
	snap := m.Snapshot()

	issues2 := &fakeIssues{}
	m2 := New(issues2, &fakeExec{}, Options{}, t.TempDir(), nil)
	m2.Now = func() time.Time { return t0 }
	m2.Restore(snap)
	if inc := m2.incidents[Key{"o/r", "A", "m1"}]; inc == nil || inc.IssueNumber != 1 || inc.State != StateDiagnosing {
		t.Fatalf("恢复后事故表：%+v", inc)
	}
	// 恢复后同键复发 → 去重（不再开 issue）
	_ = m2.Handle(context.Background(), "o/r", actA("m1", "r"))
	if len(issues2.created) != 0 {
		t.Fatal("恢复后同键活跃事故应去重")
	}
}
