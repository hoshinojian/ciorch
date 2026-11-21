package incident

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/observe"
)

// ---------- 诊断轮的 fakes ----------

type fakeLLM struct {
	calls    int
	systems  []string
	users    []string
	response string
	err      error
}

func (f *fakeLLM) Complete(ctx context.Context, system, user string) (string, error) {
	f.calls++
	f.systems = append(f.systems, system)
	f.users = append(f.users, user)
	if f.err != nil {
		return "", f.err
	}
	return f.response, nil
}

type fakeEvidence struct {
	obs      observe.Obs
	hasObs   bool
	state    decide.RepoState
	diags    map[string]string
	logs     map[int64]string
	reruns   map[int64]bool
	diagRepo map[string]string
}

func (f *fakeEvidence) Obs(repo string) (observe.Obs, bool) { return f.obs, f.hasObs }
func (f *fakeEvidence) RepoState(repo string) decide.RepoState {
	return f.state
}
func (f *fakeEvidence) MachineDiag(ctx context.Context, repo, m string) json.RawMessage {
	if d, ok := f.diags[m]; ok {
		return json.RawMessage(d)
	}
	return json.RawMessage(`{"error":"ssh 不可达"}`)
}
func (f *fakeEvidence) JobLogTail(ctx context.Context, repo string, jobID int64) string {
	return f.logs[jobID]
}
func (f *fakeEvidence) RerunnableRuns(ctx context.Context, repo string) map[int64]bool {
	return f.reruns
}

func obsWithMachine() observe.Obs {
	o := observe.Obs{Now: t0, Complete: true}
	o.Machines = []machine.Machine{{Pool: "do-sgp1-c2", ID: "1", Name: "ci-nms-team2-ci-sgp1-01", CreatedAt: t0.Add(-time.Hour)}}
	o.Runners = []observe.Runner{{ID: 1, Name: "ci-nms-team2-ci-sgp1-01", Status: "online"}}
	o.Jobs = []observe.Job{{ID: 5, RunID: 100, Status: "in_progress", RunnerName: "ci-nms-team2-ci-sgp1-01", StartedAt: t0.Add(-70 * time.Minute), CreatedAt: t0.Add(-75 * time.Minute)}}
	return o
}

func newDiagMgr(t *testing.T, llm *fakeLLM, ev *fakeEvidence) (*Manager, *fakeIssues, *fakeExec, *testClock) {
	t.Helper()
	issues, exec := &fakeIssues{}, &fakeExec{}
	m, clock := newMgr(t, issues, exec)
	m.LLM = llm
	m.Evidence = ev
	m.Opt.PoolsOf = func(repo string) []string { return []string{"do-sgp1-c2"} }
	return m, issues, exec, clock
}

// ---------- §8.1 incident：诊断轮 ----------

// 合法结论 → 执行动作 → verifying；诊断评论与动作结果评论各一条。
func TestDiagnoseValidActionExecutes(t *testing.T) {
	llm := &fakeLLM{response: `{"root_cause":"Listener 挂起","confidence":0.9,"evidence":["online 不接活"],
		"action":"restart_runner","action_args":{"machine":"ci-nms-team2-ci-sgp1-01"},
		"needs_human":false,"human_steps":"","issue_comment":"重启可解"}`}
	ev := &fakeEvidence{obs: obsWithMachine(), hasObs: true}
	m, issues, exec, _ := newDiagMgr(t, llm, ev)

	_ = m.Handle(context.Background(), "o/r", actA("ci-nms-team2-ci-sgp1-01", "假活"))
	m.Tick(context.Background(), "o/r")

	if llm.calls != 1 {
		t.Fatalf("应调一次 LLM：%d", llm.calls)
	}
	if !strings.Contains(llm.systems[0], "restart_runner") || !strings.Contains(llm.systems[0], "json") {
		t.Fatalf("系统提示词应含白名单与 json 字样")
	}
	if !strings.Contains(llm.users[0], `"incident"`) || !strings.Contains(llm.users[0], "ci-nms-team2-ci-sgp1-01") {
		t.Fatalf("用户消息应是诊断材料 JSON（含机器名）")
	}
	if len(exec.restarts) != 1 || exec.restarts[0] != "ci-nms-team2-ci-sgp1-01" {
		t.Fatalf("应执行 restart_runner：%v", exec.restarts)
	}
	inc := m.incidents[Key{"o/r", "A", "ci-nms-team2-ci-sgp1-01"}]
	if inc.State != StateVerifying {
		t.Fatalf("动作执行后应 verifying：%s", inc.State)
	}
	joined := strings.Join(issues.comments[1], "\n---\n")
	if !strings.Contains(joined, "重启可解") || !strings.Contains(joined, "已执行动作") {
		t.Fatalf("诊断与动作结果评论：%v", issues.comments[1])
	}
	// verifying 稳定 2 轮 → resolved
	m.Tick(context.Background(), "o/r")
	m.Tick(context.Background(), "o/r")
	if inc.State != StateResolved {
		t.Fatalf("verifying 安静 2 轮应 resolved：%s", inc.State)
	}
}

// needs_human 结论 → waiting-human + needs-human 标签 + @维护者 + human_steps。
func TestDiagnoseNeedsHuman(t *testing.T) {
	llm := &fakeLLM{response: `{"root_cause":"DO 账户欠费","confidence":0.8,"evidence":["402"],
		"action":"none","action_args":{},"needs_human":true,"human_steps":"充值并检查账号","issue_comment":"账户层问题"}`}
	ev := &fakeEvidence{obs: obsWithMachine(), hasObs: true}
	m, issues, _, _ := newDiagMgr(t, llm, ev)

	_ = m.Handle(context.Background(), "o/r", actA("m", "r"))
	m.Tick(context.Background(), "o/r")
	inc := m.incidents[Key{"o/r", "A", "m"}]
	if inc.State != StateWaitingHuman {
		t.Fatalf("needs_human 应 waiting-human：%s", inc.State)
	}
	if len(issues.addedLbl[1]) == 0 || issues.addedLbl[1][0] != "needs-human" {
		t.Fatalf("应打 needs-human 标签：%v", issues.addedLbl[1])
	}
	last := issues.comments[1][len(issues.comments[1])-1]
	if !strings.Contains(last, "@maintainer") || !strings.Contains(last, "充值并检查账号") {
		t.Fatalf("评论应 @维护者并带 human_steps：%q", last)
	}
	// waiting-human 不自动恢复（§3.10.6）
	m.Tick(context.Background(), "o/r")
	m.Tick(context.Background(), "o/r")
	m.Tick(context.Background(), "o/r")
	if inc.State != StateWaitingHuman {
		t.Fatal("waiting-human 不得被安静恢复")
	}
	// 人工关闭 → resolved
	m.HumanResolved("o/r", 1)
	if inc.State != StateResolved {
		t.Fatal("人工关闭应 resolved")
	}
}

// 结论非法（非 JSON / 越权）：降级 needs_human，评论附原文。
func TestDiagnoseInvalidVerdict(t *testing.T) {
	for name, resp := range map[string]string{
		"非 JSON": `这不是 JSON`,
		"越权机器":   `{"root_cause":"r","confidence":0.9,"action":"drain_droplet","action_args":{"machine":"not-ours"},"needs_human":false,"issue_comment":"c"}`,
	} {
		llm := &fakeLLM{response: resp}
		ev := &fakeEvidence{obs: obsWithMachine(), hasObs: true}
		m, issues, _, _ := newDiagMgr(t, llm, ev)
		_ = m.Handle(context.Background(), "o/r", actA("m", "r"))
		m.Tick(context.Background(), "o/r")
		inc := m.incidents[Key{"o/r", "A", "m"}]
		if inc.State != StateWaitingHuman {
			t.Fatalf("%s：应降级 waiting-human：%s", name, inc.State)
		}
		last := issues.comments[1][len(issues.comments[1])-1]
		if !strings.Contains(last, "结论不合法") || !strings.Contains(last, "@maintainer") {
			t.Fatalf("%s：评论应注明不合法并 @维护者：%q", name, last)
		}
	}
}

// 调用限额：每事故 3 次（失败也计）；耗尽 → waiting-human + issue 注明。
func TestLLMLimitPerIncident(t *testing.T) {
	llm := &fakeLLM{err: errors.New("网络超时")}
	ev := &fakeEvidence{obs: obsWithMachine(), hasObs: true}
	m, issues, _, _ := newDiagMgr(t, llm, ev)

	_ = m.Handle(context.Background(), "o/r", actA("m", "r"))
	// 判据每轮仍在（Handle 重发）——否则安静恢复会先收尾（设计如此）
	for i := 0; i < 3; i++ {
		m.Tick(context.Background(), "o/r")
		_ = m.Handle(context.Background(), "o/r", actA("m", "r"))
	}
	if llm.calls != 3 {
		t.Fatalf("应恰好调 3 次：%d", llm.calls)
	}
	// 判据仍在（再发一次）→ 下一轮：轮次已满（3 ≥ 上限 3）→ waiting-human + issue 注明上限
	_ = m.Handle(context.Background(), "o/r", actA("m", "r"))
	m.Tick(context.Background(), "o/r")
	inc := m.incidents[Key{"o/r", "A", "m"}]
	if inc.State != StateWaitingHuman {
		t.Fatalf("轮次耗尽应 waiting-human：%s", inc.State)
	}
	last := issues.comments[1][len(issues.comments[1])-1]
	if !strings.Contains(last, "上限") {
		t.Fatalf("issue 应注明已达上限：%q", last)
	}
}

// 调用限额：每日 20 次（跨事故共享，跨日清零）。
func TestLLMLimitPerDay(t *testing.T) {
	ok := `{"root_cause":"r","confidence":0.9,"action":"none","action_args":{},"needs_human":false,"issue_comment":"c"}`
	ev := &fakeEvidence{obs: obsWithMachine(), hasObs: true}
	m, _, _, clock := newDiagMgr(t, &fakeLLM{response: ok}, ev)

	// 19 个不同主体各 1 轮（19 次调用）+ 1 个到 1 次 = 20；第 21 次被拒
	for i := 0; i < 20; i++ {
		_ = m.Handle(context.Background(), "o/r", actA(string(rune('a'+i)), "r"))
	}
	m.Tick(context.Background(), "o/r") // 一轮推进全部 diagnosing（20 次调用内）
	llm := m.LLM.(*fakeLLM)
	if llm.calls != 20 {
		t.Fatalf("当日应恰好 20 次：%d", llm.calls)
	}
	// 第 21 个事故：waiting-human（日限额）
	_ = m.Handle(context.Background(), "o/r", actA("zzz", "r"))
	m.Tick(context.Background(), "o/r")
	inc := m.incidents[Key{"o/r", "A", "zzz"}]
	if inc.State != StateWaitingHuman {
		t.Fatalf("日限额耗尽应 waiting-human：%s", inc.State)
	}
	// 跨日清零
	clock.t = clock.t.Add(25 * time.Hour)
	_ = m.Handle(context.Background(), "o/r", actA("yyy", "r"))
	m.Tick(context.Background(), "o/r")
	if got := m.incidents[Key{"o/r", "A", "yyy"}].State; got != StateVerifying && got != StateDiagnosing {
		t.Fatalf("跨日应恢复可诊断：%s", got)
	}
}

// G 型（外部 runner 假活）：只告知，不调 LLM，直接 waiting-human。
func TestScenarioGNoLLM(t *testing.T) {
	llm := &fakeLLM{response: `{"action":"none"}`}
	ev := &fakeEvidence{obs: obsWithMachine(), hasObs: true}
	m, _, _, _ := newDiagMgr(t, llm, ev)

	_ = m.Handle(context.Background(), "o/r", decide.Action{Kind: decide.ActIncident, IncidentType: "G", IncidentKey: "gitrunner-sgp2", Reason: "外部"})
	m.Tick(context.Background(), "o/r")
	if llm.calls != 0 {
		t.Fatal("G 型不应调 LLM")
	}
	if inc := m.incidents[Key{"o/r", "G", "gitrunner-sgp2"}]; inc.State != StateWaitingHuman {
		t.Fatalf("G 应直接 waiting-human：%s", inc.State)
	}
}

// DryRun：不调 LLM、无 issue 副作用；diagnosing 停留，由安静恢复收尾。
func TestDiagnoseDryRun(t *testing.T) {
	llm := &fakeLLM{response: `{"action":"none"}`}
	ev := &fakeEvidence{obs: obsWithMachine(), hasObs: true}
	m, issues, exec, _ := newDiagMgr(t, llm, ev)
	m.DryRun = true

	_ = m.Handle(context.Background(), "o/r", actA("m", "r"))
	m.Tick(context.Background(), "o/r")
	if llm.calls != 0 {
		t.Fatal("dry-run 不得调 LLM")
	}
	if len(exec.restarts)+len(exec.drains)+len(exec.pauses) != 0 {
		t.Fatal("dry-run 不得执行动作")
	}
	if len(issues.created)+len(issues.comments) != 0 {
		t.Fatal("dry-run 不得开/评 issue")
	}
}

// 材料预算：约 100KB，超限按 job_log_tail → machine_diag → timeline 截断。
func TestMaterialBudget(t *testing.T) {
	big := strings.Repeat("x", 200*1024)
	ev := &fakeEvidence{
		obs: obsWithMachine(), hasObs: true,
		diags: map[string]string{"ci-nms-team2-ci-sgp1-01": `{"checks":{},"logs":"` + big + `"}`},
		logs:  map[int64]string{5: big},
	}
	m, _, _, _ := newDiagMgr(t, &fakeLLM{}, ev)
	inc := &Incident{Repo: "o/r", Type: "D", Subject: "ci-nms-team2-ci-sgp1-01", FirstSeen: t0}
	for i := 0; i < 40; i++ {
		m.Note("o/r", "observe", strings.Repeat("e", 4000))
	}
	raw, err := m.buildMaterial(context.Background(), inc, 1, ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > materialBudget+4096 {
		t.Fatalf("材料应截到 ~100KB：%d", len(raw))
	}
	if !strings.Contains(string(raw), `"truncated"`) && !strings.Contains(string(raw), `"job_log_tail":""`) {
		t.Fatalf("截断应发生（job_log_tail 清空或 diag 截断标记）：%d", len(raw))
	}
}

// 证据字段：timeline/Q/I/N/熔断/machine_diag/previous_rounds。
func TestMaterialFields(t *testing.T) {
	ev := &fakeEvidence{
		obs:    obsWithMachine(),
		hasObs: true,
		state:  decide.RepoState{Breaker: decide.Breaker{State: "open", Fails: 3}, CreateFailStreak: 2},
		reruns: map[int64]bool{4242: true},
	}
	m, _, _, _ := newDiagMgr(t, &fakeLLM{}, ev)
	m.Note("o/r", "observe", "Q=3")
	inc := &Incident{Repo: "o/r", Type: "A", Subject: "ci-nms-team2-ci-sgp1-01", FirstSeen: t0}
	raw, err := m.buildMaterial(context.Background(), inc, 2, ev, []previousRound{{ActionResult: "已重启"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{`"timeline"`, `"repo_state"`, `"open"`, `"machine_diag"`, `"ssh 不可达"`, `"previous_rounds"`, "已重启", `"jobs"`, `"runners"`, `"machines"`} {
		if !strings.Contains(s, want) {
			t.Errorf("材料应含 %q", want)
		}
	}
}

// 每日计数持久化：Restore 后当日额度不清零。
func TestLLMUsagePersist(t *testing.T) {
	m, _, _, _ := newDiagMgr(t, &fakeLLM{}, &fakeEvidence{})
	day := t0.UTC().Format("2006-01-02")
	m.RestoreLLM(day, 19)
	if d, c := m.LLMUsage(); d != day || c != 19 {
		t.Fatalf("LLMUsage：%v %v", d, c)
	}
}
