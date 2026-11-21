package decide

import (
	"fmt"
	"strconv"
	"time"
)

// ApplyTuning 把 config 的 global.tuning 覆盖到决策参数（键名与默认值见
// REDESIGN §3.6.6）。严格校验：未知键或非法值（非正时长/整数）即报错——
// 配置错误须在启动期首轮 reconcile 暴露，不带病运行。poll_active/poll_idle
// 归 daemon 侧轮询用（本包无对应字段）：只校验合法性，不存储。
func (p *Params) ApplyTuning(m map[string]string) error {
	durs := map[string]*time.Duration{
		"stall_queue_age":     &p.StallQueueAge,
		"stall_min_online":    &p.StallMinOnline,
		"stall_restart_grace": &p.StallRestartGrace,
		"cold_start_timeout":  &p.ColdStartTimeout,
		"job_stuck_alert":     &p.JobStuckAlert,
		"job_stuck_kill":      &p.JobStuckKill,
		"idle_timeout":        &p.IdleTimeout,
		"idle_timeout_busy":   &p.IdleTimeoutBusy,
		"max_lifetime":        &p.MaxLifetime,
		"breaker_cooldown":    &p.BreakerCooldown,
		"create_backoff_max":  &p.CreateBackoffMax,
	}
	cnts := map[string]*int{
		"stall_samples":      &p.StallSamples,
		"offline_samples":    &p.OfflineSamples,
		"breaker_max_fails":  &p.BreakerMaxFails,
		"observe_fail_alert": &p.ObserveFailAlert,
	}
	for k, v := range m {
		switch {
		case k == "poll_active" || k == "poll_idle":
			if d, err := time.ParseDuration(v); err != nil || d <= 0 {
				return fmt.Errorf("tuning %s=%q: 须为正时长（daemon 轮询间隔）", k, v)
			}
		case durs[k] != nil:
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("tuning %s=%q: %w", k, v, err)
			}
			if d <= 0 {
				return fmt.Errorf("tuning %s=%q: 须为正时长", k, v)
			}
			*durs[k] = d
		case cnts[k] != nil:
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("tuning %s=%q: %w", k, v, err)
			}
			if n <= 0 {
				return fmt.Errorf("tuning %s=%q: 须为正整数", k, v)
			}
			*cnts[k] = n
		default:
			return fmt.Errorf("tuning 未知键 %q（键名见 REDESIGN §3.6.6）", k)
		}
	}
	return nil
}
