package incident

import (
	"strings"
	"testing"
)

func validVC() ValidateContext {
	return ValidateContext{
		OwnedMachines:  []string{"ci-nms-team2-ci-sgp1-01"},
		ProfilePools:   []string{"do-sgp1-c2"},
		RerunnableRuns: map[int64]bool{4242: true},
	}
}

func validVerdict() Verdict {
	return Verdict{
		RootCause: "Listener 挂起", Confidence: 0.9, Evidence: []string{"runner online 但 busy=false"},
		Action: "restart_runner", ActionArgs: ActionArgs{Machine: "ci-nms-team2-ci-sgp1-01"},
		IssueComment: "结论：重启",
	}
}

// §8.1：每一种非法情况都降级 needs_human。
func TestVerdictValidationDowngrades(t *testing.T) {
	cases := []struct {
		name   string
		typ    string // 校验目标场景（默认 A）
		mutate func(*Verdict)
		want   string
	}{
		{"未知动作", "", func(v *Verdict) { v.Action = "rm -rf" }, "白名单"},
		{"场景外动作（A 不许 pause_repo）", "", func(v *Verdict) { v.Action = "pause_repo"; v.ActionArgs = ActionArgs{DurationMin: 60} }, "白名单"},
		{"机器越权", "", func(v *Verdict) { v.ActionArgs.Machine = "someone-else-01" }, "不是本仓可下手"},
		{"机器不存在", "", func(v *Verdict) { v.ActionArgs.Machine = "ci-nms-team2-ci-sgp1-99" }, "不是本仓可下手"},
		{"pool 不在白名单", "B", func(v *Verdict) { v.Action = "switch_pool"; v.ActionArgs = ActionArgs{Pool: "do-blr1-c2"} }, "白名单"},
		{"duration 越下界", "B", func(v *Verdict) { v.Action = "pause_repo"; v.ActionArgs = ActionArgs{DurationMin: 5} }, "[10,360]"},
		{"duration 越上界", "B", func(v *Verdict) { v.Action = "pause_repo"; v.ActionArgs = ActionArgs{DurationMin: 361} }, "[10,360]"},
		{"run_id 无 runner 丢失特征", "D", func(v *Verdict) { v.Action = "rerun_failed_jobs"; v.ActionArgs = ActionArgs{RunID: 777} }, "runner 丢失特征"},
		{"低置信度降级", "", func(v *Verdict) { v.Confidence = 0.4 }, "低置信度"},
		{"confidence 越界", "", func(v *Verdict) { v.Confidence = 1.5 }, "[0,1]"},
		{"缺 root_cause", "", func(v *Verdict) { v.RootCause = " " }, "root_cause"},
	}
	for _, c := range cases {
		typ := c.typ
		if typ == "" {
			typ = "A"
		}
		v := validVerdict()
		if typ != "A" {
			v.Confidence = 0.9 // 换场景时保持其他字段合法
		}
		c.mutate(&v)
		got, reason := v.Validate(typ, validVC())
		if reason == "" || !strings.Contains(reason, c.want) {
			t.Errorf("%s：应降级且理由含 %q，got %q", c.name, c.want, reason)
		}
		if !got.NeedsHuman || got.Action != "none" {
			t.Errorf("%s：降级后应 needs_human + none，got %+v", c.name, got)
		}
	}
}

// 合法结论按原样通过（不降级）。
func TestVerdictValidPasses(t *testing.T) {
	v := validVerdict()
	got, reason := v.Validate("A", validVC())
	if reason != "" {
		t.Fatalf("合法结论不应降级：%s", reason)
	}
	if got.Action != "restart_runner" || got.NeedsHuman {
		t.Fatalf("结论被误改：%+v", got)
	}
	// B 场景的 switch_pool 合法路径（单池白名单命中）
	b := Verdict{RootCause: "r", Confidence: 0.8, Action: "switch_pool", ActionArgs: ActionArgs{Pool: "do-sgp1-c2"}}
	if _, reason := b.Validate("B", validVC()); reason != "" {
		t.Fatalf("B 的合法 switch_pool 不应降级：%s", reason)
	}
	// rerun 合法路径
	d := Verdict{RootCause: "r", Confidence: 0.8, Action: "rerun_failed_jobs", ActionArgs: ActionArgs{RunID: 4242}}
	if _, reason := d.Validate("D", validVC()); reason != "" {
		t.Fatalf("D 的合法 rerun 不应降级：%s", reason)
	}
}

// E 场景只允许 none（配置错误只能建议人改配置）。
func TestScenarioEOnlyNone(t *testing.T) {
	v := validVerdict() // restart_runner
	if _, reason := v.Validate("E", validVC()); reason == "" {
		t.Fatal("E 场景任何非 none 动作都应被拒")
	}
	none := Verdict{RootCause: "label 配错", Confidence: 0.9, Action: "none"}
	if _, reason := none.Validate("E", validVC()); reason != "" {
		t.Fatalf("E 的 none 不应被拒：%s", reason)
	}
}

// 非法 JSON 解析报错（调用方按 needs_human 处理）。
func TestParseVerdictBadJSON(t *testing.T) {
	if _, err := ParseVerdict("这不是 JSON"); err == nil {
		t.Fatal("非 JSON 应报错")
	}
	if v, err := ParseVerdict(`{"root_cause":"r","confidence":0.9,"action":"none"}`); err != nil || v.Action != "none" {
		t.Fatalf("合法 JSON 应解析：%v %+v", err, v)
	}
}
