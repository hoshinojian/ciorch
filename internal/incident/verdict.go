// 结论 schema 与白名单校验（REDESIGN §3.10.5）。任一不过 → 视为
// needs_human=true、action=none；低置信度（<0.5 且 action≠none）降级为
// none + needs_human。校验依据（本仓所辖机器、profile.pools 白名单、
// rerun 特征）由证据采集阶段填进 ValidateContext。
package incident

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ActionArgs 是结论的动作参数（§3.10.5 schema）。
type ActionArgs struct {
	Machine     string `json:"machine"`
	Pool        string `json:"pool"`
	DurationMin int    `json:"duration_min"`
	RunID       int64  `json:"run_id"`
}

// Verdict 是 LLM 结论（解析自 JSON 模式输出）。
type Verdict struct {
	RootCause    string     `json:"root_cause"`
	Confidence   float64    `json:"confidence"`
	Evidence     []string   `json:"evidence"`
	Action       string     `json:"action"`
	ActionArgs   ActionArgs `json:"action_args"`
	NeedsHuman   bool       `json:"needs_human"`
	HumanSteps   string     `json:"human_steps"`
	IssueComment string     `json:"issue_comment"`
}

// ParseVerdict 解析模型输出（非法 JSON 返回错误，调用方按 needs_human 处理）。
func ParseVerdict(raw string) (Verdict, error) {
	var v Verdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return Verdict{}, fmt.Errorf("结论不是合法 JSON: %w", err)
	}
	return v, nil
}

// allowedActions 是各场景允许的动作子集（§3.10.5 表；switch_pool 白名单 =
// profile.pools，当前单池配置下实际不可迁移）。G（外部 runner 假活）只告知，
// 不给动作。
var allowedActions = map[string][]string{
	"A": {"none", "restart_runner", "replace_droplet", "drain_droplet"},
	"B": {"none", "switch_pool", "clear_breaker", "pause_repo"},
	"C": {"none", "replace_droplet"},
	"D": {"none", "drain_droplet", "rerun_failed_jobs"},
	"E": {"none"},
	"F": {"none", "drain_droplet"},
	"H": {"none", "pause_repo"},
	"I": {"none", "pause_repo"},
	"J": {"none"},
	"G": {"none"},
}

// AllowedActions 返回场景允许的动作（提示词与校验同源）。
func AllowedActions(incidentType string) []string {
	return allowedActions[incidentType]
}

// ValidateContext 是校验依据（证据采集阶段填好）。
type ValidateContext struct {
	// OwnedMachines 是本仓所辖现存机器名（machine 越权判定）。
	OwnedMachines []string
	// ProfilePools 是该仓 profile.pools（switch_pool 白名单；当前长度 1）。
	ProfilePools []string
	// RerunnableRuns 是"近 24h 且失败 job 日志含 runner 丢失特征"的 run id 集
	//（rerun_failed_jobs 的 run_id 越权判定）。
	RerunnableRuns map[int64]bool
}

// Validate 校验结论并按需降级。返回降级说明（空串 = 按结论执行）；
// 返回的 Verdict 已按规则修正（非法/越权 → NeedsHuman=true、Action="none"）。
func (v Verdict) Validate(incidentType string, vc ValidateContext) (Verdict, string) {
	downgrade := func(reason string) (Verdict, string) {
		v.NeedsHuman = true
		v.Action = "none"
		return v, reason
	}
	if strings.TrimSpace(v.RootCause) == "" {
		return downgrade("结论缺 root_cause")
	}
	if v.Confidence < 0 || v.Confidence > 1 {
		return downgrade(fmt.Sprintf("confidence %v 不在 [0,1]", v.Confidence))
	}
	allowed := false
	for _, a := range allowedActions[incidentType] {
		if a == v.Action {
			allowed = true
			break
		}
	}
	if !allowed {
		return downgrade(fmt.Sprintf("动作 %q 不在场景 %s 的白名单 %v 内", v.Action, incidentType, allowedActions[incidentType]))
	}
	switch v.Action {
	case "restart_runner", "replace_droplet", "drain_droplet":
		if !containsStr(vc.OwnedMachines, v.ActionArgs.Machine) {
			return downgrade(fmt.Sprintf("machine %q 不是本仓可下手的机器（须在当前观察里、未标 Bad、不在删除流程中；可下手：%v）", v.ActionArgs.Machine, vc.OwnedMachines))
		}
	case "switch_pool":
		if !containsStr(vc.ProfilePools, v.ActionArgs.Pool) {
			return downgrade(fmt.Sprintf("pool %q 不在该仓 profile.pools 白名单 %v 内", v.ActionArgs.Pool, vc.ProfilePools))
		}
	case "pause_repo":
		if v.ActionArgs.DurationMin < 10 || v.ActionArgs.DurationMin > 360 {
			return downgrade(fmt.Sprintf("duration_min %d 不在 [10,360]", v.ActionArgs.DurationMin))
		}
	case "rerun_failed_jobs":
		if !vc.RerunnableRuns[v.ActionArgs.RunID] {
			return downgrade(fmt.Sprintf("run_id %d 不是本仓近 24h 带 runner 丢失特征的失败 run（可重跑：%v）", v.ActionArgs.RunID, keysOf(vc.RerunnableRuns)))
		}
	}
	if v.Confidence < 0.5 && v.Action != "none" {
		return downgrade(fmt.Sprintf("confidence %.2f < 0.5 且动作 %q 非空，按低置信度降级", v.Confidence, v.Action))
	}
	return v, ""
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func keysOf(m map[int64]bool) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
