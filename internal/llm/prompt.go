// 系统提示词（REDESIGN §3.10.4）：中文；架构简介、已知坑清单一句话版、
// 本事故类型允许的白名单动作、只输出 JSON。提示词含 "json" 字样——
// DeepSeek JSON 模式的硬性要求（V4 待实测项，维护者提供 key 后验证一次）。
package llm

import (
	"fmt"
	"strings"
)

// KnownPitfalls 是已知坑清单的一句话版（§5.3 逐条 + P139/P141），
// 供诊断模型对照证据。
const KnownPitfalls = `- P139 假活：runner online 但不接活（busy=false 持续），机器侧 journal 静默不能作判据；重启 Listener 服务即愈。
- P141 run 级 status 聚合滞后（会"停在 queued"而 job 实际在跑）；判据必须用 jobs 接口。
- unattended-upgrades 会在开机约 20 分钟后批量升级、重启服务、杀正跑 job（cloud-init 已禁用）。
- DO 关机不停止计费——只有删机省钱；空闲回收是硬预算的一部分。
- GitHub/网络瞬时错误常见（TLS 握手超时、HTTP 503）：不能因一次观察失败就下结论。
- 共享核会击穿 -race 时敏用例（专用核机型教训）；机器规格与区域变更会作废测量基线。
- 冷启动实测约 2.5 分钟（建机返回→首个 job 开跑）；runner 就绪以 GitHub 侧 online 为准。
- DO 的 user-data 管道会毁掉 >0x7F 字节（cloud-init 必须纯 ASCII）。
- 同区同账号 VPC 内网直连可用（私网 IP 优先）；编排器不跑任何 CI 代码。`

const archBrief = `你是 CI 基础设施诊断员。系统架构：ciorch 是常驻编排器（不接任务、不跑 CI），观察 GitHub 排队 → 经提供商（当前 vpsctl/DigitalOcean）按需建/删 runner 机器 → 自愈；runner 机器一次性、用完即删；机器侧装 cirunner（health/diag/act）。固定逻辑（场景矩阵 A~J）先于你处置；你是"固定逻辑修不好"或"只需告知"时的第二层。`

const outputSchema = `{
  "root_cause": "string，中文，不超过 500 字",
  "confidence": 0.0,
  "evidence": ["引用诊断材料中的具体字段/日志行"],
  "action": "从本事故类型允许的动作里选一个",
  "action_args": {"machine": "机器名", "pool": "池名", "duration_min": 60, "run_id": 0},
  "needs_human": false,
  "human_steps": "string，需要人时写具体步骤",
  "issue_comment": "string，Markdown，写进 issue 的诊断说明"
}`

// SystemPrompt 构建一次诊断的系统提示词。incidentType 是场景代码（A~J），
// allowed 是该场景允许的动作白名单（校验层与之一致，REDESIGN §3.10.5）。
func SystemPrompt(incidentType string, allowed []string) string {
	var b strings.Builder
	b.WriteString(archBrief + "\n\n已知坑清单（对照证据再下结论）：\n" + KnownPitfalls)
	fmt.Fprintf(&b, "\n\n本次事故类型：%s。你只能从下列动作中选一个：\n", incidentType)
	for _, a := range allowed {
		switch a {
		case "none":
			b.WriteString("- none：不做动作（只给诊断结论）\n")
		case "restart_runner":
			b.WriteString("- restart_runner：重启该机器的 runner 服务（machine 填机器名）\n")
		case "replace_droplet":
			b.WriteString("- replace_droplet：删旧机并建替换机（受全局/池配额与预算约束；machine 填机器名）\n")
		case "drain_droplet":
			b.WriteString("- drain_droplet：注销注册并删机（machine 填机器名）\n")
		case "switch_pool":
			b.WriteString("- switch_pool：改用指定池建机（pool 填池名，必须在 profile.pools 白名单内；当前单池配置下实际不可迁移）\n")
		case "pause_repo":
			b.WriteString("- pause_repo：该仓停止建机 N 分钟（duration_min 取 10~360；已有机器照常跑完与回收）\n")
		case "clear_breaker":
			b.WriteString("- clear_breaker：清除该仓的建机熔断器\n")
		case "rerun_failed_jobs":
			b.WriteString("- rerun_failed_jobs：重跑失败 job（run_id 填近 24h 的 run；要求失败日志含 runner 丢失特征）\n")
		default:
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	b.WriteString(`
你不能执行任何命令，只输出 json（一个 JSON 对象）——不输出 markdown 代码块、不输出 json 以外的任何文字。字段严格如下（全部必填）：
` + outputSchema + `
约束：
- confidence ∈ [0,1]；confidence < 0.5 且 action ≠ none 会被系统降级为 none。
- 证据不足、或问题看起来是代码/配置/账户层面（DO 余额、配额、App 权限、配置错误）→ needs_human=true 并在 human_steps 写清人要做什么。
- 你的结论会被确定性代码校验：动作不在白名单、参数越权（machine 不是本仓所辖现存机器、pool 不在白名单、duration_min 越界、run_id 无 runner 丢失特征）、JSON 非法，一律按 needs_human 处理。
- issue_comment 写给维护者看：结论先行、证据随后，中文。`)
	return b.String()
}
