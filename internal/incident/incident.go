// Package incident 是事故管线（REDESIGN §3.10）：状态机、去重、issue 读写、
// incidents.jsonl、人工介入入口。诊断（LLM）与白名单动作在 verdict.go/actions
// 相关文件扩展（阶段 5 后续 PR）；本文件先落状态机骨架与 issue 规范。
//
// 固定逻辑（场景矩阵）先于本模块执行；本模块是"固定逻辑修不好"或"需要
// 告知"时的第二层。A/B/C 单次发生且被固定逻辑修好的：不开 issue、只写
// incidents.jsonl 并计数（NoteRemedy 入口）。
package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/githubapp"
)

// 状态机（§3.10.1）。diagnosing 之后的 acting/verifying 由诊断 PR 接上。
const (
	StateDetected     = "detected"
	StateDiagnosing   = "diagnosing"
	StateActing       = "acting"
	StateVerifying    = "verifying"
	StateWaitingHuman = "waiting-human"
	StateResolved     = "resolved"
)

// reuseWindow 是同键事故复用 issue 的时间窗（§3.10.1：24h）。
const reuseWindow = 24 * time.Hour

// quietRoundsToResolve 是"判据不再成立且稳定"的轮数（§3.10.1：稳定 2 轮）。
const quietRoundsToResolve = 2

// maxCommentChars 是单条 issue 评论上限（§3.10.2：60000，超长截断保留末尾）。
const maxCommentChars = 60000

// timelineKeep 是时间线保留条数（§3.10.2 正文：最近 20 条）。
const timelineKeep = 20

// scenarioNames 场景代码 → 中文名（issue 标题）。
var scenarioNames = map[string]string{
	"A": "runner 假活", "B": "冷启失败", "C": "掉线", "D": "job 卡死",
	"E": "label 不匹配", "F": "配额不足", "G": "外部 runner 假活",
	"H": "观察失败", "I": "建机报错", "J": "孤儿机器",
}

// ScenarioName 返回场景中文名（未知代码原样返回）。
func ScenarioName(code string) string {
	if n, ok := scenarioNames[code]; ok {
		return n
	}
	return code
}

// Key 是事故键：(仓, 场景类型, 主体)。
type Key struct {
	Repo    string
	Type    string
	Subject string
}

// Incident 是一条事故的跨轮记录（持久化形态同结构）。
type Incident struct {
	Repo, Type, Subject string
	State               string // 上述状态常量
	IssueNumber         int64  // 复用的 issue（0=未开）
	IssueRepo           string // issue 所在仓（issue_repo 改投）
	FirstSeen           time.Time
	LastSeen            time.Time
	ResolvedAt          time.Time
	QuietRounds         int             // Tick 累计的连续未复发轮数
	LLMRounds           int             // 已用诊断轮次（限额每事故 3）
	PreviousRounds      []previousRound // 历轮结论与动作结果（下一轮证据的一部分）
}

// Entry 是时间线一条（observe | action | result）。
type Entry struct {
	T      time.Time
	Event  string
	Detail string
}

// IssuesAPI 是事故模块需要的 GitHub issue 能力（*githubapp.Client 实现）。
type IssuesAPI interface {
	EnsureLabels(ctx context.Context, repo string, labels []string) error
	CreateIssue(ctx context.Context, repo, title, body string, labels []string) (githubapp.Issue, error)
	AddIssueComment(ctx context.Context, repo string, number int64, body string) error
	AddIssueLabels(ctx context.Context, repo string, number int64, labels []string) error
	CloseIssue(ctx context.Context, repo string, number int64) error
	ReopenIssue(ctx context.Context, repo string, number int64) error
}

// Options 是 Manager 的配置。
type Options struct {
	MaintainerLogin   string
	IssueRepoOf       func(repo string) string // 空 = 在本仓开（§3.3 issue_repo）
	MaxLLMPerIncident int                      // 默认 3（§3.10.4）
	MaxLLMPerDay      int                      // 默认 20
	// PoolsOf 返回该仓 profile.pools（switch_pool 白名单校验；装配层注入）。
	PoolsOf func(repo string) []string
}

// Manager 是事故模块入口（engine.OnIncident 接 Handle；daemon 每轮接 Tick）。
type Manager struct {
	Issues IssuesAPI
	Exec   Executor
	Opt    Options
	Now    func() time.Time
	Log    *log.Logger
	// LLM 是诊断客户端（nil = 不诊断：diagnosing 停留至安静恢复）。
	LLM LLMClient
	// Evidence 是诊断材料采集面（nil = 无证据，诊断不可用）。
	Evidence EvidenceSource
	// DryRun（shadow）：只记日志与 jsonl，不开 issue、不调 LLM、不执行动作。
	DryRun bool

	mu        sync.Mutex
	incidents map[Key]*Incident
	timeline  map[string][]Entry  // repo → 最近 timelineKeep 条
	remedies  map[Key][]time.Time // 近 24h 固定逻辑补救时刻（独立于事故记录——补救不开 issue，不能污染 24h issue 复用判定）
	jsonlPath string

	llmDayUTC   string // 每日 LLM 调用计数（§3.10.4 限额；持久化）
	llmDayCount int
}

// New 构造。stateDir 为空则不落 jsonl（测试用）。
func New(issues IssuesAPI, exec Executor, opt Options, stateDir string, lg *log.Logger) *Manager {
	if lg == nil {
		lg = log.New(os.Stderr, "", log.LstdFlags)
	}
	if opt.MaxLLMPerIncident == 0 {
		opt.MaxLLMPerIncident = 3
	}
	if opt.MaxLLMPerDay == 0 {
		opt.MaxLLMPerDay = 20
	}
	m := &Manager{
		Issues: issues, Exec: exec, Opt: opt, Log: lg,
		Now:       time.Now,
		incidents: map[Key]*Incident{},
		timeline:  map[string][]Entry{},
		remedies:  map[Key][]time.Time{},
	}
	if stateDir != "" {
		m.jsonlPath = filepath.Join(stateDir, "incidents.jsonl")
	}
	return m
}

// Handle 处理一条事故动作（engine.OnIncident 入口）。
// 去重（shadow 教训：场景 H 每轮重发）：同键活跃事故只更新 LastSeen/时间线，
// 不重复开 issue、不重复推进状态机。
func (m *Manager) Handle(ctx context.Context, repo string, a decide.Action) error {
	if a.IncidentType == "" {
		return fmt.Errorf("事故动作缺场景类型：%+v", a)
	}
	key := Key{Repo: repo, Type: a.IncidentType, Subject: a.IncidentKey}
	now := m.Now()
	m.mu.Lock()
	m.note(repo, "observe", fmt.Sprintf("事故 %s/%s：%s", a.IncidentType, a.IncidentKey, a.Reason))
	inc := m.incidents[key]
	if inc != nil && inc.State != StateResolved {
		inc.LastSeen = now
		inc.QuietRounds = 0
		m.mu.Unlock()
		return nil // 活跃事故去重：连续每轮重发只算一次
	}
	reuse := inc != nil && now.Sub(inc.ResolvedAt) <= reuseWindow
	if inc == nil {
		inc = &Incident{Repo: repo, Type: a.IncidentType, Subject: a.IncidentKey, FirstSeen: now}
		m.incidents[key] = inc
	}
	inc.LastSeen, inc.QuietRounds, inc.LLMRounds = now, 0, 0
	inc.State = StateDetected
	m.mu.Unlock()

	if m.DryRun {
		m.Log.Printf("[incident][%s] dry-run %s/%s（不开 issue）：%s", repo, a.IncidentType, a.IncidentKey, a.Reason)
		m.appendJSONL(now, repo, a.IncidentType, a.IncidentKey, "detected", a.Reason)
		m.mu.Lock()
		inc.State = StateDiagnosing // shadow 下也推进状态（Tick 可恢复），只是无副作用
		m.mu.Unlock()
		return nil
	}

	issueRepo := repo
	if m.Opt.IssueRepoOf != nil {
		if r := m.Opt.IssueRepoOf(repo); r != "" {
			issueRepo = r
		}
	}
	inc.IssueRepo = issueRepo
	title := fmt.Sprintf("[ciorch] %s：%s（%s）", ScenarioName(a.IncidentType), a.IncidentKey, repo)
	labels := []string{"ciorch", "ciorch:" + strings.ToLower(a.IncidentType)}

	if reuse {
		// 同键 24h 内再次出现：复用 issue（reopen + 追加评论），不开新的
		if err := m.Issues.ReopenIssue(ctx, issueRepo, inc.IssueNumber); err != nil {
			m.Log.Printf("[incident][%s] 重开 issue #%d 失败: %v", repo, inc.IssueNumber, err)
		}
		m.comment(ctx, inc, fmt.Sprintf("同键事故 %.0f 小时内再次出现，复用本 issue。\n\n%s",
			m.Now().Sub(inc.ResolvedAt).Hours(), a.Reason))
	} else {
		if err := m.Issues.EnsureLabels(ctx, issueRepo, labels); err != nil {
			m.Log.Printf("[incident][%s] 补建标签失败: %v", repo, err)
		}
		is, err := m.Issues.CreateIssue(ctx, issueRepo, title, m.issueBody(inc, a.Reason), labels)
		if err != nil {
			return fmt.Errorf("开 issue: %w", err)
		}
		inc.IssueNumber = is.Number
	}
	m.appendJSONL(now, repo, a.IncidentType, a.IncidentKey, "opened", a.Reason)
	m.mu.Lock()
	inc.State = StateDiagnosing
	m.mu.Unlock()
	m.Log.Printf("[incident][%s] %s 已开 issue #%d（%s）", repo, title, inc.IssueNumber, issueRepo)
	return nil
}

// Tick 每轮调用（daemon）：先推进诊断轮（diagnosing → acting → verifying），
// 再做安静恢复——连续 quietRoundsToResolve 轮未复发 → resolved（评论依据
// 并关闭）。waiting-human 不自动恢复（§3.10.6：只有人工关闭 issue 才 resolved）。
func (m *Manager) Tick(ctx context.Context, repo string) {
	now := m.Now()
	m.diagnoseTick(ctx, repo)
	m.mu.Lock()
	var toResolve []*Incident
	for _, inc := range m.incidents {
		if inc.Repo != repo || inc.State == StateResolved || inc.State == StateWaitingHuman {
			continue
		}
		inc.QuietRounds++
		if inc.QuietRounds >= quietRoundsToResolve {
			inc.State = StateResolved
			inc.ResolvedAt = now
			toResolve = append(toResolve, inc)
		}
	}
	m.mu.Unlock()
	for _, inc := range toResolve {
		basis := fmt.Sprintf("判据连续 %d 轮未再出现（场景 %s/%s）", quietRoundsToResolve, inc.Type, inc.Subject)
		if m.DryRun {
			m.Log.Printf("[incident][%s] dry-run 已恢复：%s", repo, basis)
			m.appendJSONL(now, repo, inc.Type, inc.Subject, "resolved", basis)
			continue
		}
		m.comment(ctx, inc, "已恢复："+basis)
		if inc.IssueNumber != 0 {
			if err := m.Issues.CloseIssue(ctx, inc.IssueRepo, inc.IssueNumber); err != nil {
				m.Log.Printf("[incident][%s] 关 issue #%d 失败: %v", repo, inc.IssueNumber, err)
			}
		}
		m.appendJSONL(now, repo, inc.Type, inc.Subject, "resolved", basis)
		m.Log.Printf("[incident][%s] 事故 %s/%s 已恢复并关闭 issue #%d", repo, inc.Type, inc.Subject, inc.IssueNumber)
	}
}

// NoteRemedy 记录固定逻辑的自动补救（A/B/C 单次发生且修好：不开 issue、
// 只写 incidents.jsonl 并计数；§3.10.1）。计数独立于事故记录——补救不建
// Incident（否则会误触发"24h 内复用 issue"，而它从没开过 issue）。
func (m *Manager) NoteRemedy(repo, scenario, subject, detail string) {
	now := m.Now()
	key := Key{Repo: repo, Type: scenario, Subject: subject}
	m.mu.Lock()
	m.remedies[key] = pruneTimes(append(m.remedies[key], now), now)
	m.note(repo, "result", "固定逻辑补救 "+scenario+"/"+subject+"："+detail)
	m.mu.Unlock()
	m.appendJSONL(now, repo, scenario, subject, "remedy", detail)
}

// remedyCount 返回该键近 24h 的补救次数（issue 正文用）。
func (m *Manager) remedyCount(key Key) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(pruneTimes(m.remedies[key], m.Now()))
}

func pruneTimes(ts []time.Time, now time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) <= reuseWindow {
			out = append(out, t)
		}
	}
	return out
}

// HumanResolved 人工关闭 issue 后的入口（webhook issues.closed → server 调用；
// §3.10.6）：事故 resolved + 清熔断/解除 pause/CreateFailStreak=0。
func (m *Manager) HumanResolved(repo string, issueNumber int64) {
	now := m.Now()
	m.mu.Lock()
	var hit *Incident
	for _, inc := range m.incidents {
		if inc.Repo == repo && inc.IssueNumber == issueNumber {
			hit = inc
			break
		}
	}
	if hit == nil {
		m.mu.Unlock()
		m.Log.Printf("[incident][%s] issue #%d 无对应事故（已清理或他因关闭）", repo, issueNumber)
		return
	}
	hit.State = StateResolved
	hit.ResolvedAt = now
	m.mu.Unlock()
	if m.Exec != nil {
		m.Exec.HumanCleared(repo)
	}
	m.appendJSONL(now, repo, hit.Type, hit.Subject, "human", fmt.Sprintf("人工关闭 issue #%d", issueNumber))
	m.Log.Printf("[incident][%s] 人工关闭 issue #%d → 事故 %s/%s resolved，已清熔断/退避/pause", repo, issueNumber, hit.Type, hit.Subject)
}

// Note 向时间线追加一条（daemon 每轮摘要、engine 动作结果由装配层喂入）。
func (m *Manager) Note(repo, event, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.note(repo, event, detail)
}

func (m *Manager) note(repo, event, detail string) {
	m.timeline[repo] = append(m.timeline[repo], Entry{T: m.Now(), Event: event, Detail: detail})
	if len(m.timeline[repo]) > timelineKeep {
		m.timeline[repo] = m.timeline[repo][len(m.timeline[repo])-timelineKeep:]
	}
}

// Snapshot/Restore：事故表持久化（daemon state.json；§3.12）。
func (m *Manager) Snapshot() []Incident {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Incident, 0, len(m.incidents))
	for _, inc := range m.incidents {
		out = append(out, *inc)
	}
	return out
}

func (m *Manager) Restore(incs []Incident) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range incs {
		inc := incs[i]
		m.incidents[Key{inc.Repo, inc.Type, inc.Subject}] = &inc
	}
}

// comment 追加评论（60K 截断保留末尾，§3.10.2）。
func (m *Manager) comment(ctx context.Context, inc *Incident, body string) {
	if inc.IssueNumber == 0 {
		return
	}
	if n := len([]rune(body)); n > maxCommentChars {
		r := []rune(body)
		cut := fmt.Sprintf("\n\n…（前 %d 字符已截断，保留末尾）\n", n-maxCommentChars)
		body = cut + string(r[n-maxCommentChars:])
	}
	if err := m.Issues.AddIssueComment(ctx, inc.IssueRepo, inc.IssueNumber, body); err != nil {
		m.Log.Printf("[incident][%s] 评论 issue #%d 失败: %v", inc.Repo, inc.IssueNumber, err)
	}
}

// issueBody 正文模板（§3.10.2 四段：发生了什么+影响 / 时间线 / 固定逻辑处置 / 下一步）。
func (m *Manager) issueBody(inc *Incident, reason string) string {
	m.mu.Lock()
	tl := append([]Entry(nil), m.timeline[inc.Repo]...)
	m.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "## 发生了什么\n\n%s：%s（%s）\n\n%s\n\n", ScenarioName(inc.Type), inc.Subject, inc.Repo, reason)
	b.WriteString("## 时间线（最近 20 条相关观察与动作）\n\n| 时间 | 事件 | 详情 |\n|---|---|---|\n")
	for i := len(tl) - 1; i >= 0 && i >= len(tl)-timelineKeep; i-- {
		e := tl[i]
		fmt.Fprintf(&b, "| %s | %s | %s |\n", e.T.Format("01-02 15:04:05"), e.Event, strings.ReplaceAll(e.Detail, "|", "\\|"))
	}
	b.WriteString("\n## 固定逻辑已做的处置\n\n（见时间线 action/result 条目；补救计数：" +
		fmt.Sprintf("场景 %s 近 24h 固定逻辑补救 %d 次）\n", inc.Type, m.remedyCount(Key{inc.Repo, inc.Type, inc.Subject})))
	b.WriteString("\n## 下一步\n\n正在调用大模型诊断（结论将追加为评论）。\n")
	return b.String()
}

// appendJSONL 追加事故日志（state_dir/incidents.jsonl，§3.12；失败只记日志）。
func (m *Manager) appendJSONL(t time.Time, repo, typ, subject, event, detail string) {
	if m.jsonlPath == "" {
		return
	}
	rec := struct {
		T       time.Time `json:"t"`
		Repo    string    `json:"repo"`
		Type    string    `json:"type"`
		Subject string    `json:"subject"`
		Event   string    `json:"event"`
		Detail  string    `json:"detail"`
	}{t, repo, typ, subject, event, detail}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.jsonlPath), 0o755); err != nil {
		m.Log.Printf("[incident] jsonl 目录失败: %v", err)
		return
	}
	f, err := os.OpenFile(m.jsonlPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		m.Log.Printf("[incident] 打开 jsonl 失败: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		m.Log.Printf("[incident] 写 jsonl 失败: %v", err)
	}
}
