// 诊断轮（REDESIGN §3.10.1/§3.10.4/§3.10.5）：diagnosing →（LLM 结论+校验）
// → acting（执行白名单动作）→ verifying；needs_human/轮次用尽/结论非法 →
// waiting-human。调用限额：每事故 MaxLLMPerIncident（默认 3）、每日
// MaxLLMPerDay（默认 20）；超限直接 waiting-human 并在 issue 注明。
package incident

import (
	"context"
	"fmt"
	"time"

	"github.com/hoshinojian/ciorch/internal/llm"
)

// LLMClient 是诊断调用面（*llm.Client 实现；测试用假实现——无真实 key）。
type LLMClient interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// Executor（诊断轮扩展）：白名单动作的执行面（engine 实现）。
// 动作仍受 §3.7 配额/预算约束（replace 无配额时执行面自行降级为 drain 后再建）。
type Executor interface {
	// HumanCleared 人工关闭 issue 后的清理：清熔断、解除 pause、CreateFailStreak=0。
	HumanCleared(repo string)
	// RestartRunner 经 ssh 重启机器上的 runner 服务。
	RestartRunner(ctx context.Context, repo, machine string) error
	// DrainMachine 注销注册并删机。
	DrainMachine(ctx context.Context, repo, machine string) error
	// ReplaceMachine 建替换机（受配额/预算约束；无配额降级 drain 后再建）。
	ReplaceMachine(ctx context.Context, repo, machine string) error
	// PauseRepo 该仓停止建机 d 分钟（已有机器照常跑完与回收）。
	PauseRepo(repo string, d time.Duration)
	// ClearBreaker 清该仓建机熔断。
	ClearBreaker(repo string)
	// SwitchPool 临时改用指定池建机（单池配置下不可迁移，校验层已挡）。
	SwitchPool(repo, pool string, d time.Duration)
	// RerunFailedJobs 重跑 run 的失败 job。
	RerunFailedJobs(ctx context.Context, repo string, runID int64) error
}

// diagnoseTick 处理 diagnosing 状态的事故：一轮一次诊断推进。
// Tick（daemon 每轮）在安静恢复检查之后调用。
func (m *Manager) diagnoseTick(ctx context.Context, repo string) {
	now := m.Now()
	m.mu.Lock()
	var toDiagnose []*Incident
	for _, inc := range m.incidents {
		if inc.Repo != repo || inc.State != StateDiagnosing {
			continue
		}
		if m.DryRun || m.LLM == nil {
			continue // shadow/未接线：停在 diagnosing，由安静恢复收尾
		}
		// G（外部 runner 假活）只告知：不诊断，直接 waiting-human
		if inc.Type == "G" {
			inc.State = StateWaitingHuman
			m.mu.Unlock()
			m.enterWaitingHuman(ctx, inc, "外部 runner 不归本编排器所辖，只告知不诊断；如需处理请人工介入", "")
			m.mu.Lock()
			continue
		}
		toDiagnose = append(toDiagnose, inc)
	}
	m.mu.Unlock()
	for _, inc := range toDiagnose {
		m.diagnoseRound(ctx, inc, now)
	}
}

// diagnoseRound 推进一个事故的一轮诊断。
func (m *Manager) diagnoseRound(ctx context.Context, inc *Incident, now time.Time) {
	// 限额：每事故 / 每日（§3.10.4）
	m.mu.Lock()
	if inc.LLMRounds >= m.Opt.MaxLLMPerIncident {
		inc.State = StateWaitingHuman
		m.mu.Unlock()
		m.enterWaitingHuman(ctx, inc,
			fmt.Sprintf("已达大模型调用上限（每事故 %d 次），转人工", m.Opt.MaxLLMPerIncident), "")
		return
	}
	if m.llmDayUTC != now.UTC().Format("2006-01-02") {
		m.llmDayUTC, m.llmDayCount = now.UTC().Format("2006-01-02"), 0
	}
	if m.llmDayCount >= m.Opt.MaxLLMPerDay {
		inc.State = StateWaitingHuman
		m.mu.Unlock()
		m.enterWaitingHuman(ctx, inc,
			fmt.Sprintf("已达大模型每日调用上限（%d 次），转人工", m.Opt.MaxLLMPerDay), "")
		return
	}
	inc.LLMRounds++
	round := inc.LLMRounds
	m.llmDayCount++
	ev := m.Evidence
	llmClient := m.LLM
	prev := append([]previousRound(nil), inc.PreviousRounds...)
	m.mu.Unlock()

	raw, err := m.buildMaterial(ctx, inc, round, ev, prev)
	if err != nil {
		m.Log.Printf("[incident][%s] 证据采集失败: %v", inc.Repo, err)
		m.comment(ctx, inc, fmt.Sprintf("第 %d 轮证据采集失败：%v（下一轮重试）", round, err))
		return
	}
	out, err := llmClient.Complete(ctx, llm.SystemPrompt(inc.Type, AllowedActions(inc.Type)), string(raw))
	if err != nil {
		m.Log.Printf("[incident][%s] 诊断调用失败: %v", inc.Repo, err)
		m.appendJSONL(now, inc.Repo, inc.Type, inc.Subject, "llm", "调用失败: "+err.Error())
		m.comment(ctx, inc, fmt.Sprintf("第 %d 轮诊断调用失败：%v", round, err))
		return // 留在 diagnosing，下一轮重试（轮次已计，耗尽走限额分支）
	}
	m.appendJSONL(now, inc.Repo, inc.Type, inc.Subject, "llm", "第 "+fmt.Sprint(round)+" 轮结论已出")

	verdict, perr := ParseVerdict(out)
	if perr != nil {
		m.toWaitingHumanInvalid(ctx, inc, verdict, out, perr)
		return
	}
	vc := m.validateContext(ctx, inc, ev)
	v, downgrade := verdict.Validate(inc.Type, vc)
	if downgrade != "" {
		m.toWaitingHumanInvalid(ctx, inc, v, out, fmt.Errorf("%s", downgrade))
		return
	}

	// 诊断结论评论（§3.10.2：每次 LLM 诊断追加一条评论）
	comment := v.IssueComment
	if comment == "" {
		comment = fmt.Sprintf("第 %d 轮诊断：根因=%s（置信度 %.2f）", round, v.RootCause, v.Confidence)
	}
	if v.NeedsHuman {
		m.mu.Lock()
		inc.State = StateWaitingHuman
		inc.PreviousRounds = append(inc.PreviousRounds, previousRound{Verdict: &v, ActionResult: "needs_human"})
		m.mu.Unlock()
		m.enterWaitingHuman(ctx, inc, comment, v.HumanSteps)
		return
	}

	// acting：执行白名单动作 → verifying
	result := m.executeAction(ctx, inc, v)
	m.mu.Lock()
	inc.PreviousRounds = append(inc.PreviousRounds, previousRound{Verdict: &v, ActionResult: result})
	inc.State = StateVerifying
	m.mu.Unlock()
	m.comment(ctx, inc, comment+"\n\n**已执行动作** `"+v.Action+"`："+result)
	m.appendJSONL(now, inc.Repo, inc.Type, inc.Subject, "action", v.Action+" → "+result)
	m.Log.Printf("[incident][%s] 第 %d 轮结论 action=%s：%s", inc.Repo, round, v.Action, result)
}

// validateContext 组装校验依据（本仓所辖机器 / profile.pools / 可重跑 run）。
func (m *Manager) validateContext(ctx context.Context, inc *Incident, ev EvidenceSource) ValidateContext {
	vc := ValidateContext{RerunnableRuns: map[int64]bool{}, ProfilePools: m.poolsOf(inc.Repo)}
	if ev == nil {
		return vc
	}
	if obs, ok := ev.Obs(inc.Repo); ok {
		for _, mc := range obs.Machines {
			vc.OwnedMachines = append(vc.OwnedMachines, mc.Name)
		}
	}
	vc.RerunnableRuns = ev.RerunnableRuns(ctx, inc.Repo)
	return vc
}

// executeAction 执行白名单动作（受执行面约束；§3.10.5）。
func (m *Manager) executeAction(ctx context.Context, inc *Incident, v Verdict) string {
	if m.Exec == nil {
		return "执行面未接线（动作未执行）"
	}
	switch v.Action {
	case "none":
		return "无需动作，进入观察（verifying）"
	case "restart_runner":
		if err := m.Exec.RestartRunner(ctx, inc.Repo, v.ActionArgs.Machine); err != nil {
			return "失败：" + err.Error()
		}
		return "已重启 " + v.ActionArgs.Machine + " 的 runner 服务"
	case "drain_droplet":
		if err := m.Exec.DrainMachine(ctx, inc.Repo, v.ActionArgs.Machine); err != nil {
			return "失败：" + err.Error()
		}
		return "已回收 " + v.ActionArgs.Machine
	case "replace_droplet":
		if err := m.Exec.ReplaceMachine(ctx, inc.Repo, v.ActionArgs.Machine); err != nil {
			return "失败：" + err.Error()
		}
		return "已替换 " + v.ActionArgs.Machine + "（配额/预算约束下）"
	case "pause_repo":
		m.Exec.PauseRepo(inc.Repo, time.Duration(v.ActionArgs.DurationMin)*time.Minute)
		return fmt.Sprintf("该仓停建 %d 分钟（已有机器照常跑完与回收）", v.ActionArgs.DurationMin)
	case "clear_breaker":
		m.Exec.ClearBreaker(inc.Repo)
		return "已清建机熔断"
	case "switch_pool":
		m.Exec.SwitchPool(inc.Repo, v.ActionArgs.Pool, 6*time.Hour)
		return "已临时（6h）改用池 " + v.ActionArgs.Pool + " 建机"
	case "rerun_failed_jobs":
		if err := m.Exec.RerunFailedJobs(ctx, inc.Repo, v.ActionArgs.RunID); err != nil {
			return "失败：" + err.Error()
		}
		return fmt.Sprintf("已重跑 run %d 的失败 job", v.ActionArgs.RunID)
	default:
		return "未知动作（未执行）：" + v.Action
	}
}

// toWaitingHuman 结论非法：needs_human + issue 注明并附原文（§3.10.5）。
func (m *Manager) toWaitingHumanInvalid(ctx context.Context, inc *Incident, v Verdict, raw string, why error) {
	m.mu.Lock()
	inc.State = StateWaitingHuman
	inc.PreviousRounds = append(inc.PreviousRounds, previousRound{Verdict: nil, ActionResult: "invalid"})
	m.mu.Unlock()
	body := fmt.Sprintf("**大模型结论不合法**（%v），按 needs_human 处理。\n\n原结论：\n\n```\n%s\n```", why, raw)
	m.enterWaitingHuman(ctx, inc, body, "请人工确认诊断结论后处理（合法动作见 issue 标签对应的场景白名单）。")
	m.appendJSONL(m.Now(), inc.Repo, inc.Type, inc.Subject, "llm", "结论不合法: "+why.Error())
}

// enterWaitingHuman 打 needs-human 标签、评论 @维护者（§3.10.6）。
// steps 是人要做的事（human_steps 或固定文案）。
func (m *Manager) enterWaitingHuman(ctx context.Context, inc *Incident, comment, steps string) {
	if m.DryRun {
		m.Log.Printf("[incident][%s] dry-run waiting-human：%s %s", inc.Repo, comment, steps)
		m.appendJSONL(m.Now(), inc.Repo, inc.Type, inc.Subject, "waiting-human", comment)
		return
	}
	if inc.IssueNumber != 0 {
		label := "needs-human"
		if err := m.Issues.EnsureLabels(ctx, inc.IssueRepo, []string{label}); err != nil {
			m.Log.Printf("[incident][%s] 补建 needs-human 失败: %v", inc.Repo, err)
		}
		if err := m.Issues.AddIssueLabels(ctx, inc.IssueRepo, inc.IssueNumber, []string{label}); err != nil {
			m.Log.Printf("[incident][%s] 打 needs-human 失败: %v", inc.Repo, err)
		}
	}
	at := ""
	if m.Opt.MaintainerLogin != "" {
		at = "@" + m.Opt.MaintainerLogin + " "
	}
	if steps == "" {
		steps = "请人工介入处理。"
	}
	m.comment(ctx, inc, comment+"\n\n"+at+steps)
	m.appendJSONL(m.Now(), inc.Repo, inc.Type, inc.Subject, "waiting-human", comment)
	m.Log.Printf("[incident][%s] 事故 %s/%s 转 waiting-human", inc.Repo, inc.Type, inc.Subject)
}

// LLMUsage/RestoreLLM：每日调用计数的持久化（跨重启不清零当日额度）。
func (m *Manager) LLMUsage() (dayUTC string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.llmDayUTC, m.llmDayCount
}

func (m *Manager) RestoreLLM(dayUTC string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.llmDayUTC, m.llmDayCount = dayUTC, count
}

// poolsOf 由装配层注入的池白名单查询单仓 profile.pools（校验 switch_pool 用）。
func (m *Manager) poolsOf(repo string) []string {
	if m.Opt.PoolsOf != nil {
		return m.Opt.PoolsOf(repo)
	}
	return nil
}
