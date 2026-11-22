package incident

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/observe"
)

// §3.10.3 recent_vpsctl_errors / unmatched_queued 必须真的进材料：
// 冒烟 issue #1/#3 的 LLM 误判根源就是材料里没有建机错误原文与不可满足
// 排队的 label 组合（2026-11-21）。
func TestMaterialIncludesProviderErrorsAndUnmatched(t *testing.T) {
	ev := &fakeEvidence{hasObs: true}
	ev.obs = observe.Obs{Now: t0, Complete: true}
	ev.obs.Machines = []machine.Machine{{Pool: "do", ID: "1", Name: "m1", CreatedAt: t0.Add(-time.Hour)}}
	ev.obs.UnmatchedQueued = []observe.Job{{
		ID: 9, RunID: 4, Status: "queued",
		Labels:    []string{"nonexistent-label", "self-hosted"},
		CreatedAt: t0.Add(-6 * time.Minute),
	}}
	ev.state = decide.RepoState{RecentProviderErrors: []decide.ProviderError{
		{At: t0.Add(-time.Minute), Op: "create", Err: "渲染 cloud-init: 公钥 \"ssh-ed25519 AAAA k\\n\" 含引号/换行等危险字符"},
		{At: t0.Add(-30 * time.Second), Op: "delete", Err: "vpsctl delete: 500 内部错误"},
	}}
	m, _ := newMgr(t, &fakeIssues{}, &fakeExec{})

	raw, err := m.buildMaterial(context.Background(), &Incident{Repo: "o/r", Type: "E"}, 1, ev, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		RecentProviderErrors []string            `json:"recent_vpsctl_errors"`
		UnmatchedQueued      []materialUnmatched `json:"unmatched_queued"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.RecentProviderErrors) != 2 ||
		!strings.Contains(got.RecentProviderErrors[0], "create: 渲染 cloud-init") ||
		!strings.Contains(got.RecentProviderErrors[1], "delete: vpsctl delete") {
		t.Fatalf("recent_vpsctl_errors 应含建机/删机错误原文：%v", got.RecentProviderErrors)
	}
	if len(got.UnmatchedQueued) != 1 || got.UnmatchedQueued[0].ID != 9 ||
		strings.Join(got.UnmatchedQueued[0].Labels, ",") != "nonexistent-label,self-hosted" ||
		!strings.HasPrefix(got.UnmatchedQueued[0].QueuedAge, "6m") {
		t.Fatalf("unmatched_queued 应含 label 组合与排队时长：%+v", got.UnmatchedQueued)
	}
}

// actionableMachines：当前观察里仍在、未标 Bad、不在删除流程（replacing/
// draining）中的机器才是 verdict 机器类动作的合法目标；restarting 保留。
// 冒烟 S3：LLM 对已被固定逻辑强删（Bad+replacing）的机器选 replace_droplet
// 仍被放行——lastObs 滞后一个轮询，但 Bad 标记当轮就在状态里。
func TestActionableMachinesExcludesBadAndDeleting(t *testing.T) {
	o := observe.Obs{Now: t0, Complete: true}
	o.Machines = []machine.Machine{
		{Name: "m-healthy"}, {Name: "m-bad"}, {Name: "m-replacing"}, {Name: "m-draining"}, {Name: "m-restarting"},
	}
	st := decide.RepoState{Machines: map[string]decide.MachineState{
		"m-bad":        {Bad: true},
		"m-replacing":  {Remedy: "replacing"},
		"m-draining":   {Remedy: "draining"},
		"m-restarting": {Remedy: "restarting"},
	}}
	got := strings.Join(actionableMachines(o, st), ",")
	if got != "m-healthy,m-restarting" {
		t.Fatalf("可下手机器应只剩健康与重启中的，got %q", got)
	}
}

// 端到端：validateContext 把 Bad 机器从 OwnedMachines 剔除 → verdict 指向
// 它的 replace_droplet 被降级 none + needs_human。
func TestValidateContextDowngradesActionOnBadMachine(t *testing.T) {
	ev := &fakeEvidence{hasObs: true}
	ev.obs = obsWithMachine() // 机器 ci-nms-team2-ci-sgp1-01
	ev.state = decide.RepoState{Machines: map[string]decide.MachineState{
		"ci-nms-team2-ci-sgp1-01": {Bad: true, Remedy: "replacing"},
	}}
	m, _ := newMgr(t, &fakeIssues{}, &fakeExec{})
	inc := &Incident{Repo: "o/r", Type: "C", Subject: "ci-nms-team2-ci-sgp1-01"}

	vc := m.validateContext(context.Background(), inc, ev)
	if len(vc.OwnedMachines) != 0 {
		t.Fatalf("Bad 机器不应在可下手列表：%v", vc.OwnedMachines)
	}
	v := Verdict{RootCause: "掉线", Confidence: 0.9, Action: "replace_droplet",
		ActionArgs: ActionArgs{Machine: "ci-nms-team2-ci-sgp1-01"}}
	out, why := v.Validate("C", vc)
	if out.Action != "none" || !out.NeedsHuman || !strings.Contains(why, "不在删除流程") {
		t.Fatalf("指向已判死机器的动作应降级 none：action=%q why=%q", out.Action, why)
	}
}
