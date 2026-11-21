package decide

import (
	"testing"
	"time"
)

// ApplyTuning 覆盖 §3.6.6 决策键；覆盖后 fillDefaults 不再回填（0 值字段
// 才回填），且非法键/值必须报错（2026-11-21 冒烟 S5 前置检查发现 tuning
// 是死配置，本文件补上解析）。
func TestApplyTuningOverridesSurviveDefaults(t *testing.T) {
	p := Params{}
	if err := p.ApplyTuning(map[string]string{
		"job_stuck_alert": "3m", "job_stuck_kill": "5m",
		"stall_samples": "5", "cold_start_timeout": "6m",
	}); err != nil {
		t.Fatal(err)
	}
	p.fillDefaults()
	if p.JobStuckAlert != 3*time.Minute || p.JobStuckKill != 5*time.Minute {
		t.Fatalf("job_stuck 覆盖未生效: %v/%v", p.JobStuckAlert, p.JobStuckKill)
	}
	if p.StallSamples != 5 {
		t.Fatalf("stall_samples 覆盖未生效: %v", p.StallSamples)
	}
	if p.ColdStartTimeout != 6*time.Minute {
		t.Fatalf("cold_start_timeout 覆盖未生效: %v", p.ColdStartTimeout)
	}
	// 未覆盖的字段仍是默认
	if p.IdleTimeout != defaultIdleTimeout || p.BreakerMaxFails != defaultBreakerMaxFails {
		t.Fatalf("未覆盖字段被改动: %v/%v", p.IdleTimeout, p.BreakerMaxFails)
	}
}

func TestApplyTuningRejectsBadInput(t *testing.T) {
	cases := []map[string]string{
		{"unknown_key": "1m"},       // 未知键
		{"job_stuck_alert": "abc"},  // 坏时长
		{"job_stuck_alert": "0s"},   // 非正时长
		{"stall_samples": "x"},      // 坏整数
		{"stall_samples": "0"},      // 非正整数
		{"poll_active": "nope"},     // poll 键值也须合法（daemon 用）
		{"breaker_cooldown": "-1m"}, // 负时长
	}
	for _, m := range cases {
		p := Params{}
		if err := p.ApplyTuning(m); err == nil {
			t.Fatalf("应报错：%v", m)
		}
	}
}

// poll_active/poll_idle 归 daemon 侧使用：decide 校验值但不存储，重复调用
// 不影响决策字段。
func TestApplyTuningValidatesPollKeysWithoutStoring(t *testing.T) {
	p := Params{}
	if err := p.ApplyTuning(map[string]string{"poll_active": "5s", "poll_idle": "30s"}); err != nil {
		t.Fatal(err)
	}
	p.fillDefaults()
	if p.StallQueueAge != defaultStallQueueAge {
		t.Fatal("poll 键不应改动决策字段")
	}
}
