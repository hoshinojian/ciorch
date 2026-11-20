// 场景处理器（§3.6.5 自愈矩阵）与扩缩容（§3.6.3）。
// 全部只读写传入的 RepoState/Obs，不做 IO；每处升级判据的依据写在 Reason。
package decide

import (
	"time"

	"github.com/hoshinojian/ciorch/internal/observe"
)

// firstRunnerID 取机器首个注册的 runner id（Drain/ForceDelete 注销用；
// rpd=2 的其余实例由 engine 按名清理）。
func firstRunnerID(m *machineView) int64 {
	for _, r := range m.runners {
		return r.ID
	}
	return 0
}

// scenarioC 掉线：曾 online、当前 offline（或注册消失）连续 OfflineSamples 轮
// → 先建后拆（同 B），不计熔断；同仓 24h 内第 2 次 → 事故。
func scenarioC(p Params, st *RepoState, obs observe.Obs, views map[string]*machineView, emit func(Action), remedied map[string]bool) {
	for name, m := range views {
		if remedied[name] {
			continue
		}
		ms := st.Machines[name]
		if ms.FirstOnlineAt.IsZero() {
			continue // 从未 online 的走 B，不走 C
		}
		if m.online > 0 || ms.OfflineSamples < p.OfflineSamples {
			continue
		}
		remedied[name] = true
		st.OfflineEvents = append(st.OfflineEvents, obs.Now)
		st.OfflineEvents = pruneEvents(st.OfflineEvents, obs.Now, stallHistoryWindow)
		// 先建后拆：有配额先建替换机，再强删旧机（掉线机注册大概率已消失；
		// engine 会把同名残留注册一并清理）
		if N := len(views); N < p.MaxRunners {
			emit(Action{Kind: ActCreate, Reason: "场景 C 掉线：先建替换机（不让队列空窗）"})
		}
		ms.Bad = true
		ms.Remedy, ms.RemedyAt = "replacing", obs.Now
		st.Machines[name] = ms
		emit(Action{
			Kind: ActForceDelete, DropletID: m.droplet.ID, DropletName: name, RunnerID: firstRunnerID(m),
			Reason: "场景 C 掉线：offline 连续命中，判死强删（先删机再清注册）",
		})
		if countRecent(st.OfflineEvents, obs.Now, stallHistoryWindow) >= 2 {
			emit(Action{Kind: ActIncident, IncidentType: "C", IncidentKey: name,
				Reason: "同仓 24h 内第 2 次掉线（场景 C 升级判据）"})
		}
	}
}

// scenarioB 冷启失败：droplet 年龄 ≥ ColdStartTimeout 且 runner 从未 online
// → 替换 + 删旧 + Breaker.Fails++；Fails 达上限 → open + 事故；
// open 满 cooldown → half-open（允许 1 台试探）；试探失败 → open 且冷却翻倍。
func scenarioB(p Params, st *RepoState, obs observe.Obs, views map[string]*machineView, emit func(Action), remedied map[string]bool) {
	for name, m := range views {
		if remedied[name] {
			continue
		}
		ms := st.Machines[name]
		if !ms.FirstOnlineAt.IsZero() {
			continue // 曾 online 的掉线归 C
		}
		age := obs.Now.Sub(m.droplet.CreatedAt)
		if age < p.ColdStartTimeout {
			continue // 冷启窗口内，再等
		}
		remedied[name] = true
		wasHalfOpen := st.Breaker.State == "half-open"
		if len(views) < p.MaxRunners {
			emit(Action{Kind: ActCreate, Reason: "场景 B 冷启失败：先建替换机（不让队列空窗）"})
		}
		ms.Bad = true
		ms.Remedy, ms.RemedyAt = "replacing", obs.Now
		st.Machines[name] = ms
		emit(Action{
			Kind: ActForceDelete, DropletID: m.droplet.ID, DropletName: name, RunnerID: firstRunnerID(m),
			Reason: "场景 B 冷启失败：droplet 超龄且 runner 从未上线，强删（注册从未成功或残留一并清）",
		})
		// 熔断计数（C 不计入）
		st.Breaker.Fails++
		if st.Breaker.Fails >= p.BreakerMaxFails || wasHalfOpen {
			st.Breaker.State = "open"
			st.Breaker.OpenedAt = obs.Now
			if wasHalfOpen {
				// 半开试探失败：冷却翻倍，封顶 4h
				cd := st.Breaker.Cooldown
				if cd == 0 {
					cd = p.BreakerCooldown
				}
				cd *= 2
				if cd > maxBreakerCooldown {
					cd = maxBreakerCooldown
				}
				st.Breaker.Cooldown = cd
			} else {
				st.Breaker.Cooldown = p.BreakerCooldown
			}
			emit(Action{Kind: ActIncident, IncidentType: "B", IncidentKey: name,
				Reason: "连续冷启失败达上限，熔断打开，停止建机待人工/冷却"})
		}
	}
}

// scenarioA 假活（P139）：Q>0 且 QAge ≥ StallQueueAge，该机 online、busy=false、
// 上线 ≥ StallMinOnline，连续 StallSamples 轮 → 有配额先建替换机 + SSH 重启；
// 重启后 StallRestartGrace 内接到活 → 解除；仍不接 → Bad → Drain。
func scenarioA(p Params, st *RepoState, obs observe.Obs, views map[string]*machineView, Q int, QAge time.Duration, emit func(Action), remedied map[string]bool) {
	for name, m := range views {
		ms := st.Machines[name]
		// 重启补救在途：观察窗口内解除或判死
		if ms.Remedy == "restarting" {
			if m.anyBusy() {
				ms.Remedy, ms.RemedyAt = "", time.Time{}
				ms.StallSamples = 0
				st.Machines[name] = ms
				continue
			}
			if obs.Now.Sub(ms.RemedyAt) > p.StallRestartGrace {
				ms.Bad = true
				ms.Remedy = "draining"
				st.Machines[name] = ms
				emit(Action{
					Kind: ActDrain, DropletID: m.droplet.ID, DropletName: name, RunnerID: firstRunnerID(m),
					Reason: "场景 A 假活：重启观察窗后仍不接活，判死回收（先注销后删机）",
				})
				remedied[name] = true
			}
			continue
		}
		if remedied[name] || ms.Bad {
			continue
		}
		stalled := Q > 0 && QAge >= p.StallQueueAge && m.online > 0 && !m.anyBusy() &&
			!ms.FirstOnlineAt.IsZero() && obs.Now.Sub(ms.FirstOnlineAt) >= p.StallMinOnline
		if !stalled {
			ms.StallSamples = 0
			st.Machines[name] = ms
			continue
		}
		ms.StallSamples++
		if ms.StallSamples < p.StallSamples {
			st.Machines[name] = ms
			continue
		}
		// 命中：先建后拆 + SSH 重启（同轮同时做，§3.6.5 A①②）
		remedied[name] = true
		st.StallEvents = append(st.StallEvents, obs.Now)
		st.StallEvents = pruneEvents(st.StallEvents, obs.Now, stallHistoryWindow)
		if len(views) < p.MaxRunners {
			emit(Action{Kind: ActCreate, Reason: "场景 A 假活：先建替换机（不让队列空窗）"})
		}
		ms.Remedy, ms.RemedyAt = "restarting", obs.Now
		st.Machines[name] = ms
		for _, r := range m.runners {
			if r.Status == "online" {
				emit(Action{
					Kind: ActSSHAct, DropletName: name, RunnerID: r.ID, RunnerName: r.Name,
					SSHAct: "restart-runner", Reason: "场景 A 假活（P139）：online 不接活，先重启 Listener",
				})
			}
		}
		if countRecent(st.StallEvents, obs.Now, stallHistoryWindow) >= 2 {
			emit(Action{Kind: ActIncident, IncidentType: "A", IncidentKey: name,
				Reason: "同仓 24h 内第 2 次假活（场景 A 升级判据）"})
		}
	}
}

// scenarioD job 卡死：runner busy 持续 ≥ JobStuckAlert → 事故；
// ≥ JobStuckKill → 再 ForceDelete（强删：job 已判死；告警不因击杀而省略）。
func scenarioD(p Params, st *RepoState, obs observe.Obs, views map[string]*machineView, emit func(Action), remedied map[string]bool) {
	for name, m := range views {
		ms := st.Machines[name]
		if ms.BusySince.IsZero() {
			continue
		}
		busy := obs.Now.Sub(ms.BusySince)
		if busy < p.JobStuckAlert {
			continue
		}
		emit(Action{Kind: ActIncident, IncidentType: "D", IncidentKey: name,
			Reason: "场景 D job 卡死：busy 持续超告警线"})
		if busy >= p.JobStuckKill {
			remedied[name] = true
			ms.Bad = true
			ms.Remedy, ms.RemedyAt = "draining", obs.Now
			st.Machines[name] = ms
			emit(Action{
				Kind: ActForceDelete, DropletID: m.droplet.ID, DropletName: name, RunnerID: firstRunnerID(m),
				Reason: "场景 D job 卡死：busy 超过击杀线，强删机器（先删机再清注册，job 已判死）",
			})
		}
	}
}

// scenarioE label 无人能满足：UnmatchedQueued 最老 ≥5m → 事故（配置错误）。
// 不建机（需求不可满足）。事故键 = label 组合（每组合一个，§3.6.5 E）。
func scenarioE(p Params, obs observe.Obs, emit func(Action)) {
	oldest := map[string]time.Duration{}
	for _, j := range obs.UnmatchedQueued {
		age := obs.Now.Sub(j.CreatedAt)
		key := labelKey(j.Labels)
		if age > oldest[key] {
			oldest[key] = age
		}
	}
	for key, age := range oldest {
		if age >= incidentEGrace {
			emit(Action{Kind: ActIncident, IncidentType: "E", IncidentKey: key,
				Reason: "label 组合无 profile 可满足且最老排队已超 5m（配置错误信号）"})
		}
	}
}

// scenarioG 外部 runner 假活：同 A 判据但 runner 不在本仓命名空间 → 不操作，
// 只告知（立即开事故）。
func scenarioG(p Params, obs observe.Obs, Q int, QAge time.Duration, emit func(Action)) {
	if p.OwnedRunnerRe == nil {
		return
	}
	if Q == 0 || QAge < p.StallQueueAge {
		return
	}
	// 外部 = 不匹配命名空间（外部 runner 永不出现在任何删除/ssh 动作里，§8.1）
	for _, r := range obs.Runners {
		if p.OwnedRunnerRe.MatchString(r.Name) {
			continue
		}
		if r.Status == "online" && !r.Busy {
			emit(Action{Kind: ActIncident, IncidentType: "G", IncidentKey: r.Name,
				Reason: "外部 runner 疑似假活（online 不接活），只告知不操作"})
		}
	}
}

// scenarioH 观察失败：连续 ObserveFailAlert 轮 Complete=false → 事故。
func scenarioH(p Params, st *RepoState, obs observe.Obs, emit func(Action)) {
	if obs.Complete {
		st.ObserveFailStreak = 0
		return
	}
	st.ObserveFailStreak++
	if st.ObserveFailStreak >= p.ObserveFailAlert {
		emit(Action{Kind: ActIncident, IncidentType: "H", IncidentKey: "",
			Reason: "观察连续失败超阈值（约 2.5 分钟以上），判据不可信"})
	}
}

// scaling 扩缩容（§3.6.3）。inFlightCreates 是本轮场景处置（A/B/C 的先建后拆）
// 已产出的替换机数——它们同属在途供给，扩容需求要扣除，防双重建机。
func scaling(p Params, st *RepoState, obs observe.Obs, views map[string]*machineView, Q int, QAge time.Duration, inFlightCreates int, emit func(Action)) {
	I, P, N := supply(p, *st, views, obs.Now)
	P += inFlightCreates * p.RunnersPerDroplet

	// 缩容（逐台）：online 空闲、IdleSince 满（自适应）阈值、Q==0、Complete；
	// Q>0 绝不缩容（硬寿命除外）；一轮只回收一台（最老的）。
	if Q == 0 && obs.Complete {
		timeout := p.IdleTimeout
		if countRecent(st.LastActivity, obs.Now, lastActivityWindow) >= 2 {
			timeout = p.IdleTimeoutBusy
		}
		var oldestName string
		var oldestIdle time.Time
		for name, m := range views {
			ms := st.Machines[name]
			if ms.Bad || ms.Remedy != "" {
				continue
			}
			if !m.allIdleOnline() || ms.IdleSince.IsZero() {
				continue
			}
			if obs.Now.Sub(ms.IdleSince) >= timeout && (oldestName == "" || ms.IdleSince.Before(oldestIdle)) {
				oldestName, oldestIdle = name, ms.IdleSince
			}
		}
		if oldestName != "" {
			ms := st.Machines[oldestName]
			ms.Remedy, ms.RemedyAt = "draining", obs.Now
			st.Machines[oldestName] = ms
			emit(Action{
				Kind: ActDrain, DropletID: views[oldestName].droplet.ID, DropletName: oldestName,
				RunnerID: firstRunnerID(views[oldestName]),
				Reason:   "空闲回收：无排队且 runner 空闲超时（一轮只回收最老一台）",
			})
		}
	}

	// 硬寿命：年龄 ≥ MaxLifetime 且 busy=false → Drain（Complete=false 也执行，
	// 防成本泄漏）；≥ MaxLifetime+2h 无论忙闲 → 告警（场景 D 变体）。
	for name, m := range views {
		ms := st.Machines[name]
		if ms.Bad {
			continue
		}
		age := obs.Now.Sub(m.droplet.CreatedAt)
		if age >= p.MaxLifetime && !m.anyBusy() {
			ms.Remedy, ms.RemedyAt = "draining", obs.Now
			st.Machines[name] = ms
			emit(Action{
				Kind: ActDrain, DropletID: m.droplet.ID, DropletName: name, RunnerID: firstRunnerID(m),
				Reason: "硬寿命到期：机器年龄超上限且回收窗口（观察不完整也执行，防成本泄漏）",
			})
		} else if age >= p.MaxLifetime+lifetimeOverrunAlert {
			emit(Action{Kind: ActIncident, IncidentType: "D", IncidentKey: name,
				Reason: "机器年龄超硬寿命+2h 仍在忙（场景 D 变体，需人工介入）"})
		}
	}

	// 扩容：want = ceil(clamp(Q-I-P, 0, (max_runners-N)*rpd) / rpd)，
	// 再受熔断（open 不建 / half-open 只 1 台试探）与建机退避约束。
	// 不设"给空闲 runner 一点时间先接"的宽限（§3.6.2：GitHub 派发秒级）。
	if Q == 0 {
		return
	}
	if !obs.Now.Before(st.CreateBackoffUntil) {
		st.CreateBackoffUntil = time.Time{}
	} else {
		return // 场景 I：退避中不建
	}
	switch st.Breaker.State {
	case "open":
		cd := st.Breaker.Cooldown
		if cd == 0 {
			cd = p.BreakerCooldown
		}
		if obs.Now.Sub(st.Breaker.OpenedAt) >= cd {
			st.Breaker.State = "half-open" // 冷却期满，允许 1 台试探
		} else {
			return
		}
	case "half-open":
		// 只允许 1 台试探：want 封顶 1（P 已含在途，通常自然为 1）
	}
	rpd := p.RunnersPerDroplet
	headroom := (p.MaxRunners - N) * rpd
	deficit := Q - I - P
	if deficit <= 0 || headroom <= 0 {
		return
	}
	if deficit > headroom {
		deficit = headroom
	}
	want := (deficit + rpd - 1) / rpd // 向上取整
	if st.Breaker.State == "half-open" && want > 1 {
		want = 1
	}
	for i := 0; i < want; i++ {
		emit(Action{Kind: ActCreate, Reason: "扩容：排队需求超出在线空闲与冷启在途供给"})
	}
}

func countRecent(events []time.Time, now time.Time, window time.Duration) int {
	n := 0
	for _, t := range events {
		if now.Sub(t) <= window {
			n++
		}
	}
	return n
}

// labelKey 把 label 集合规范成事故键（排序后逗号连接）。
func labelKey(labels []string) string {
	sorted := append([]string{}, labels...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	key := ""
	for i, l := range sorted {
		if i > 0 {
			key += ","
		}
		key += l
	}
	return key
}

// RecordCreateResult 由 engine 在建机动作执行后回填（场景 I）：
// 失败 → CreateFailStreak++、指数退避（1m/2m/4m/8m/15m 封顶）；
// 连续 3 次失败 → 事故。成功 → 清零。
func RecordCreateResult(p Params, st *RepoState, now time.Time, err error) (Action, bool) {
	p.fillDefaults()
	if err == nil {
		st.CreateFailStreak = 0
		st.CreateBackoffUntil = time.Time{}
		return Action{}, false
	}
	st.CreateFailStreak++
	backoff := createBackoff(st.CreateFailStreak, p.CreateBackoffMax)
	st.CreateBackoffUntil = now.Add(backoff)
	if st.CreateFailStreak == 3 { // 恰在第 3 次（后续靠事故模块 24h 同键复用）
		return Action{Kind: ActIncident, IncidentType: "I", IncidentKey: "",
			Reason: "建机连续 3 次报错（vpsctl/DO 侧异常），退避并升级"}, true
	}
	return Action{}, false
}

// createBackoff 指数退避：1m、2m、4m、8m、15m 封顶（§3.6.5 I）。
func createBackoff(streak int, max time.Duration) time.Duration {
	switch streak {
	case 1:
		return time.Minute
	case 2:
		return 2 * time.Minute
	case 3:
		return 4 * time.Minute
	case 4:
		return 8 * time.Minute
	default:
		return max
	}
}

// BreakerReset 人工/CLI 复位熔断（closed、Fails=0；§3.6.5）。
func BreakerReset(st *RepoState) {
	st.Breaker = Breaker{State: "closed"}
}
