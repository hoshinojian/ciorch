// 诊断材料采集（REDESIGN §3.10.3）：确定性代码采集，不经 LLM。
// 总量控制在约 100KB 内：超限按 job_log_tail → machine_diag 内的日志段 →
// timeline 的顺序截断。
package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/observe"
)

// materialBudget 是诊断材料总量上限（约 100KB，§3.10.3）。
const materialBudget = 100 << 10

// jobLogTailMax 是 job 日志尾部上限（32KB，§3.10.3）。
const jobLogTailMax = 32 << 10

// EvidenceSource 是证据采集面（装配层注入：观察快照 + 决策状态 + ssh diag
// + GitHub 日志/重跑判定的组合；全部确定性代码）。
type EvidenceSource interface {
	// Obs 返回该仓最近一轮观察（时间线/需求/机器的来源；false=无快照）。
	Obs(repo string) (observe.Obs, bool)
	// RepoState 返回该仓决策记忆（Q/I/P/N、熔断等）。
	RepoState(repo string) decide.RepoState
	// MachineDiag 经 ssh 调机器上 cirunner diag --json；不可达返回 {"error":...}。
	MachineDiag(ctx context.Context, repo, machine string) json.RawMessage
	// JobLogTail 返回 job 日志末尾（32KB；拿不到返回空串）。
	JobLogTail(ctx context.Context, repo string, jobID int64) string
	// RerunnableRuns 返回近 24h"失败 job 日志含 runner 丢失特征"的 run id 集
	//（verdict 校验 rerun_failed_jobs 用；特征：lost communication /
	// shutdown signal / The operation was canceled）。
	RerunnableRuns(ctx context.Context, repo string) map[int64]bool
}

// material 是诊断材料（§3.10.3 结构）。
type material struct {
	Incident    materialIncident           `json:"incident"`
	Timeline    []Entry                    `json:"timeline"`
	RepoState   materialRepoState          `json:"repo_state"`
	Jobs        []materialJob              `json:"jobs"`
	Runners     []materialRunner           `json:"runners"`
	Machines    []materialMachine          `json:"machines"`
	MachineDiag map[string]json.RawMessage `json:"machine_diag"`
	JobLogTail  string                     `json:"job_log_tail"`
	// RecentProviderErrors 是最近建机/删机失败原文（含渲染等建机前置错误；
	// 无此材料的盲诊是冒烟 issue #1/#3 误判的根源，2026-11-21）。
	RecentProviderErrors []string `json:"recent_vpsctl_errors"`
	// UnmatchedQueued 是 E 场景的不可满足排队 job（label 组合 + 排队时长）。
	UnmatchedQueued []materialUnmatched `json:"unmatched_queued"`
	PreviousRounds  []previousRound     `json:"previous_rounds"`
}

type materialIncident struct {
	Type      string    `json:"type"`
	Repo      string    `json:"repo"`
	Subject   string    `json:"subject"`
	FirstSeen time.Time `json:"first_seen"`
	Round     int       `json:"round"`
}

type materialRepoState struct {
	Q, I, P, N       int
	QAge             string
	Breaker          decide.Breaker `json:"breaker"`
	CreateFailStreak int            `json:"create_fail_streak"`
}

type materialJob struct {
	ID        int64     `json:"id"`
	RunID     int64     `json:"run_id"`
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	Labels    []string  `json:"labels"`
	CreatedAt time.Time `json:"created_at"`
}

type materialRunner struct {
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Busy   bool     `json:"busy"`
	Labels []string `json:"labels"`
}

type materialMachine struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Pool      string    `json:"pool"`
	Addr      string    `json:"addr"`
}

// materialUnmatched 是 E 场景证据：不可满足的排队 job 及其 label 组合与排队时长。
type materialUnmatched struct {
	ID        int64    `json:"id"`
	Labels    []string `json:"labels"`
	QueuedAge string   `json:"queued_age"`
}

type previousRound struct {
	Verdict      *Verdict `json:"verdict,omitempty"`
	ActionResult string   `json:"action_result"`
}

// buildMaterial 组装诊断材料并做预算截断。
func (m *Manager) buildMaterial(ctx context.Context, inc *Incident, round int, ev EvidenceSource, prev []previousRound) ([]byte, error) {
	if ev == nil {
		return nil, fmt.Errorf("未注入证据源")
	}
	mat := material{
		Incident:       materialIncident{Type: inc.Type, Repo: inc.Repo, Subject: inc.Subject, FirstSeen: inc.FirstSeen, Round: round},
		PreviousRounds: prev,
	}
	if obs, ok := ev.Obs(inc.Repo); ok {
		st := ev.RepoState(inc.Repo)
		mat.RepoState = materialRepoState{
			Q: countQueued(obs), I: countIdle(obs), P: pendingColdStart(st, obs), N: len(obs.Machines),
			QAge: ageOfOldestQueued(obs), Breaker: st.Breaker, CreateFailStreak: st.CreateFailStreak,
		}
		// recent_vpsctl_errors（§3.10.3）：建机/删机失败原文，逐条带时刻与操作
		for _, pe := range st.RecentProviderErrors {
			mat.RecentProviderErrors = append(mat.RecentProviderErrors,
				pe.At.UTC().Format(time.RFC3339)+" "+pe.Op+": "+pe.Err)
		}
		// unmatched_queued：E 场景的 label 组合与排队时长
		for _, j := range obs.UnmatchedQueued {
			mat.UnmatchedQueued = append(mat.UnmatchedQueued, materialUnmatched{
				ID: j.ID, Labels: j.Labels, QueuedAge: obs.Now.Sub(j.CreatedAt).String(),
			})
		}
		for _, j := range obs.Jobs {
			mat.Jobs = append(mat.Jobs, materialJob{ID: j.ID, RunID: j.RunID, Status: j.Status, Labels: j.Labels, CreatedAt: j.CreatedAt})
		}
		for _, r := range obs.Runners {
			mat.Runners = append(mat.Runners, materialRunner{Name: r.Name, Status: r.Status, Busy: r.Busy, Labels: r.Labels})
		}
		for _, mc := range obs.Machines {
			mat.Machines = append(mat.Machines, materialMachine{Name: mc.Name, CreatedAt: mc.CreatedAt, Pool: mc.Pool, Addr: mc.Addr})
		}
		// machine_diag：本仓所辖机器逐台 ssh 采集（主体优先）
		names := ownedMachineNames(inc, obs)
		mat.MachineDiag = map[string]json.RawMessage{}
		for _, name := range names {
			mat.MachineDiag[name] = ev.MachineDiag(ctx, inc.Repo, name)
		}
		// job_log_tail：卡死/失败 job 适用（场景 D 或有 in_progress job）
		if id := stuckJobID(inc, obs); id != 0 {
			mat.JobLogTail = tail(ev.JobLogTail(ctx, inc.Repo, id), jobLogTailMax)
		}
	}
	m.mu.Lock()
	mat.Timeline = append([]Entry(nil), m.timeline[inc.Repo]...)
	m.mu.Unlock()

	// 预算截断（§3.10.3 顺序：job_log_tail → machine_diag → timeline）
	raw, err := json.Marshal(mat)
	for err == nil && len(raw) > materialBudget {
		if !shrinkMaterial(&mat, inc.Subject) {
			break // 无可截断项
		}
		raw, err = json.Marshal(mat)
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// shrinkMaterial 按规定顺序截一步；无可截返回 false。
func shrinkMaterial(mat *material, subject string) bool {
	switch {
	case len(mat.JobLogTail) > 0: // ① job_log_tail 先砍半（<512 字节直接丢）
		mat.JobLogTail = tail(mat.JobLogTail, len(mat.JobLogTail)/2)
		if len(mat.JobLogTail) < 512 {
			mat.JobLogTail = ""
		}
	case len(mat.MachineDiag) > 0: // ② 非主体机器的 diag 先移除；只剩主体则截断标记
		for name := range mat.MachineDiag {
			if name != subject {
				delete(mat.MachineDiag, name)
				return true
			}
		}
		for name := range mat.MachineDiag {
			mat.MachineDiag[name] = json.RawMessage(`{"truncated":true}`)
		}
	case len(mat.Timeline) > 0: // ③ timeline 丢最老一条
		mat.Timeline = mat.Timeline[1:]
	default:
		return false
	}
	return true
}

// pendingColdStart 估算冷启在途 P（未 online 且未超龄的机器数；证据展示口径）。
func pendingColdStart(st decide.RepoState, obs observe.Obs) int {
	n := 0
	for _, mc := range obs.Machines {
		ms := st.Machines[mc.Name]
		if ms.FirstOnlineAt.IsZero() && !ms.Bad && obs.Now.Sub(mc.CreatedAt) <= 8*time.Minute {
			n++
		}
	}
	return n
}

func countQueued(obs observe.Obs) (n int) {
	for _, j := range obs.Jobs {
		if j.Status == "queued" {
			n++
		}
	}
	return n
}

func countIdle(obs observe.Obs) (n int) {
	for _, r := range obs.Runners {
		if r.Status == "online" && !r.Busy {
			n++
		}
	}
	return n
}

func ageOfOldestQueued(obs observe.Obs) string {
	var age time.Duration
	for _, j := range obs.Jobs {
		if j.Status == "queued" {
			if a := obs.Now.Sub(j.CreatedAt); a > age {
				age = a
			}
		}
	}
	return age.String()
}

// ownedMachineNames 返回证据要 diag 的机器名（主体优先，最多 3 台）。
func ownedMachineNames(inc *Incident, obs observe.Obs) []string {
	var out []string
	for _, mc := range obs.Machines { // 主体在前
		if inc.Subject != "" && mc.Name == inc.Subject {
			out = append(out, mc.Name)
			break
		}
	}
	for _, mc := range obs.Machines {
		if len(out) >= 3 {
			break
		}
		if mc.Name != inc.Subject {
			out = append(out, mc.Name)
		}
	}
	return out
}

// stuckJobID 取卡死场景（D）或最老 in_progress job 的 id（日志 tail 用）。
func stuckJobID(inc *Incident, obs observe.Obs) int64 {
	if inc.Type != "D" {
		return 0
	}
	var oldest int64
	var oldestAt time.Time
	for _, j := range obs.Jobs {
		if j.Status == "in_progress" && (oldest == 0 || j.StartedAt.Before(oldestAt)) {
			oldest, oldestAt = j.ID, j.StartedAt
		}
	}
	return oldest
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	b := s[len(s)-n:]
	if i := strings.IndexByte(b, '\n'); i >= 0 {
		b = b[i+1:] // 对齐到行首
	}
	return b
}
