// Package decide 是 ciorch 的纯决策核心（§3.6）：输入一轮观察（observe.Obs）
// 与上轮 RepoState，输出动作与新 RepoState。相同输入恒定输出；不读时钟、
// 不做 IO、不打日志——理由写进 Action.Reason。
//
// 场景矩阵（§3.6.5）：A 假活 / B 冷启失败 / C 掉线 / D job 卡死 / E label
// 无人能满足 / F 配额（budget 层）/ G 外部 runner 假活 / H 观察失败 /
// I 建机报错（执行结果回填，见 RecordCreateResult）/ J 孤儿机器。
// F 在 internal/budget；J 在 internal/engine——孤儿带公共 tag 但无仓 tag，
// 不出现在任何仓的 Obs 里，需全局扫描才能看见。
package decide

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hoshinojian/ciorch/internal/observe"
)

// ActionKind 是动作类型（§3.8）。
type ActionKind string

const (
	ActCreate      ActionKind = "create"              // 建机（异步，不等 IP）
	ActDrain       ActionKind = "drain"               // 先注销注册，成功后删 droplet
	ActForceDelete ActionKind = "force-delete"        // 先删 droplet 再删注册
	ActRemoveReg   ActionKind = "remove-registration" // 清 offline 残留注册
	ActSSHAct      ActionKind = "ssh-act"             // 经 ssh 调机器 cirunner act
	ActIncident    ActionKind = "incident"            // 交给 incident 模块
)

// Action 是一轮决策产生的单个动作。
type Action struct {
	Kind         ActionKind
	DropletID    string
	DropletName  string
	RunnerID     int64
	RunnerName   string
	SSHAct       string // restart-runner / stop-runner / clean-work
	IncidentType string // 场景代码 A~J
	IncidentKey  string // 事故键主体（机器名/label 组合/空）
	Reason       string
}

// MachineState 是单台机器的跨轮记忆（§3.6.1，key = droplet 名）。
type MachineState struct {
	FirstSeen      time.Time // 编排器首次看到该 droplet
	FirstOnlineAt  time.Time // runner 首次 online
	IdleSince      time.Time // 连续 busy=false 的起点；忙即清零
	StallSamples   int       // 场景 A 连续命中次数
	OfflineSamples int       // 场景 C 连续 offline 次数
	BusySince      time.Time // 当前这次 busy 的起点（场景 D）
	Remedy         string    // "" | restarting | replacing | draining
	RemedyAt       time.Time
	Bad            bool // 已判死，等待回收
}

// Breaker 是建机熔断器（§3.6.5）。
type Breaker struct {
	State    string        // closed | open | half-open
	Fails    int           // 连续冷启失败
	OpenedAt time.Time     // 本次 open 的起点
	Cooldown time.Duration // 当前冷却；回 open 时翻倍，封顶 4h（0=默认）
}

// RepoState 是一个仓的跨轮记忆（§3.6.1；事件史字段为 §3.6.5"何时升级为
// 事故"判据所需：A/C 的"24h 内第 2 次"与 H 的连续失败轮数无法从单轮观察推出）。
type RepoState struct {
	Machines           map[string]MachineState
	Breaker            Breaker
	CreateBackoffUntil time.Time
	CreateFailStreak   int
	LastActivity       []time.Time // 近 2h job 开跑时间点（自适应空闲），≤20 个
	StallEvents        []time.Time // 场景 A 触发补救的时刻
	OfflineEvents      []time.Time // 场景 C 触发处置的时刻
	ObserveFailStreak  int         // 场景 H 连续 Complete=false 轮数
}

// Params 是决策参数（§3.6.6，零值字段取默认）。
type Params struct {
	MaxRunners        int
	RunnersPerDroplet int
	// OwnedRunnerRe 匹配本仓命名空间的 runner 名（ci-<name>-<account>-<region>-NN，
	// rpd=2 时带 -a/-b 后缀）。nil = 全部视为外部（只观察不动手）。
	OwnedRunnerRe *regexp.Regexp

	StallQueueAge     time.Duration // A：最老匹配排队 job 年龄阈值，默认 2m
	StallMinOnline    time.Duration // A：runner 上线不足此时长不判，默认 60s
	StallSamples      int           // A：连续命中轮数，默认 3
	StallRestartGrace time.Duration // A：重启后观察窗口，默认 2m
	ColdStartTimeout  time.Duration // B：默认 8m
	OfflineSamples    int           // C：默认 2
	JobStuckAlert     time.Duration // D：默认 60m
	JobStuckKill      time.Duration // D：默认 90m
	IdleTimeout       time.Duration // 缩容：默认 20m
	IdleTimeoutBusy   time.Duration // 缩容（近 2h 活跃≥2 次）：默认 45m
	MaxLifetime       time.Duration // 硬寿命：默认 8h
	BreakerMaxFails   int           // 默认 3
	BreakerCooldown   time.Duration // 默认 30m
	CreateBackoffMax  time.Duration // I：退避封顶，默认 15m
	ObserveFailAlert  int           // H：默认 10 轮
}

const (
	defaultStallQueueAge    = 2 * time.Minute
	defaultStallMinOnline   = 60 * time.Second
	defaultStallSamples     = 3
	defaultStallGrace       = 2 * time.Minute
	defaultColdStartTimeout = 8 * time.Minute
	defaultOfflineSamples   = 2
	defaultJobStuckAlert    = 60 * time.Minute
	defaultJobStuckKill     = 90 * time.Minute
	defaultIdleTimeout      = 20 * time.Minute
	defaultIdleTimeoutBusy  = 45 * time.Minute
	defaultMaxLifetime      = 8 * time.Hour
	defaultBreakerMaxFails  = 3
	defaultBreakerCooldown  = 30 * time.Minute
	defaultCreateBackoffMax = 15 * time.Minute
	defaultObserveFailAlert = 10
	maxBreakerCooldown      = 4 * time.Hour
	lastActivityWindow      = 2 * time.Hour
	lastActivityKeep        = 20
	incidentDedupWindow     = 24 * time.Hour
	incidentEGrace          = 5 * time.Minute // E：最老不可满足 job ≥5m 才开事故
	lifetimeOverrunAlert    = 2 * time.Hour   // 硬寿命超限告警（场景 D 变体）
	stallHistoryWindow      = 24 * time.Hour
)

func (p *Params) fillDefaults() {
	if p.MaxRunners == 0 {
		p.MaxRunners = 1
	}
	if p.RunnersPerDroplet == 0 {
		p.RunnersPerDroplet = 1
	}
	if p.StallQueueAge == 0 {
		p.StallQueueAge = defaultStallQueueAge
	}
	if p.StallMinOnline == 0 {
		p.StallMinOnline = defaultStallMinOnline
	}
	if p.StallSamples == 0 {
		p.StallSamples = defaultStallSamples
	}
	if p.StallRestartGrace == 0 {
		p.StallRestartGrace = defaultStallGrace
	}
	if p.ColdStartTimeout == 0 {
		p.ColdStartTimeout = defaultColdStartTimeout
	}
	if p.OfflineSamples == 0 {
		p.OfflineSamples = defaultOfflineSamples
	}
	if p.JobStuckAlert == 0 {
		p.JobStuckAlert = defaultJobStuckAlert
	}
	if p.JobStuckKill == 0 {
		p.JobStuckKill = defaultJobStuckKill
	}
	if p.IdleTimeout == 0 {
		p.IdleTimeout = defaultIdleTimeout
	}
	if p.IdleTimeoutBusy == 0 {
		p.IdleTimeoutBusy = defaultIdleTimeoutBusy
	}
	if p.MaxLifetime == 0 {
		p.MaxLifetime = defaultMaxLifetime
	}
	if p.BreakerMaxFails == 0 {
		p.BreakerMaxFails = defaultBreakerMaxFails
	}
	if p.BreakerCooldown == 0 {
		p.BreakerCooldown = defaultBreakerCooldown
	}
	if p.CreateBackoffMax == 0 {
		p.CreateBackoffMax = defaultCreateBackoffMax
	}
	if p.ObserveFailAlert == 0 {
		p.ObserveFailAlert = defaultObserveFailAlert
	}
}

// NextName 计算下一台 droplet 名：现存同名空间最大序号 +1（跳号——被删机器
// 的序号不回收；§8.1 命名条目）。
func NextName(prefix, account, region string, existing []observe.Droplet) string {
	pre := fmt.Sprintf("%s-%s-%s-", prefix, account, region)
	max := 0
	for _, d := range existing {
		suffix, ok := strings.CutPrefix(d.Name, pre)
		if !ok {
			continue // 外部前缀/不同区域，不占本区序号
		}
		var n int
		if _, err := fmt.Sscanf(suffix, "%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%s%02d", pre, max+1)
}

// machineView 是 Decide 内部的机器视图。
type machineView struct {
	name    string
	droplet observe.Droplet
	runners []observe.Runner // 本命名空间内、名字对得上本机的注册
	online  int
	idle    int // online 且 busy=false
}

func (m *machineView) anyBusy() bool {
	for _, r := range m.runners {
		if r.Status == "online" && r.Busy {
			return true
		}
	}
	return false
}

func (m *machineView) allIdleOnline() bool {
	return m.online > 0 && m.idle == len(m.runners)
}

// Decide 执行一轮决策。相同 (p, st, obs) 恒定输出 (actions, newState)。
func Decide(p Params, st RepoState, obs observe.Obs) ([]Action, RepoState) {
	p.fillDefaults()
	if st.Machines == nil {
		st.Machines = map[string]MachineState{}
	}
	if st.Breaker.State == "" {
		st.Breaker.State = "closed"
	}

	var acts []Action
	emit := func(a Action) { acts = append(acts, a) }

	// 观察不完整：整轮冻结（REDESIGN §2.2.7 修订版——原"只建不删"在 droplets
	// 一路缺失时会超建，冻结更安全）。只累计场景 H；droplets 一路成功时仍执行
	// 硬寿命回收（防成本泄漏）；不建、不删（硬寿命除外）、不 ssh、不推进任何
	// 采样计数、不清理 Machines 记忆。
	if !obs.Complete {
		scenarioH(p, &st, obs, emit)
		if obs.DropletsOK {
			syncMachines(&st, obs)
			views := buildViewsFrozen(obs)
			hardLifetime(p, &st, obs, views, emit)
		}
		sort.SliceStable(acts, func(i, j int) bool { return actRank(acts[i]) < actRank(acts[j]) })
		return acts, st
	}

	Q, QAge := demand(obs)
	recordActivity(&st, obs)
	syncMachines(&st, obs)
	machines := buildViews(p, &st, obs)

	// 场景判定（先判定后汇总；同机一轮一个补救，优先级 C>B>A>D，§3.6.5）
	remedied := map[string]bool{}
	scenarioC(p, &st, obs, machines, emit, remedied)
	scenarioB(p, &st, obs, machines, emit, remedied)
	scenarioA(p, &st, obs, machines, Q, QAge, emit, remedied)
	scenarioD(p, &st, obs, machines, emit, remedied)
	scenarioG(p, obs, Q, QAge, emit)
	scenarioE(p, obs, emit)
	scenarioH(p, &st, obs, emit)

	// 扩缩容（§3.6.3；场景替换机计入在途供给，防双重建机）
	inFlight := 0
	for _, a := range acts {
		if a.Kind == ActCreate {
			inFlight++
		}
	}
	scaling(p, &st, obs, machines, Q, QAge, inFlight, emit)

	sort.SliceStable(acts, func(i, j int) bool { return actRank(acts[i]) < actRank(acts[j]) })
	return acts, st
}

// buildViewsFrozen 组装只读视图（不更新任何机器状态——冻结轮专用）。
func buildViewsFrozen(obs observe.Obs) map[string]*machineView {
	views := map[string]*machineView{}
	for _, d := range obs.Droplets {
		views[d.Name] = &machineView{name: d.Name, droplet: d}
	}
	return views
}

func actRank(a Action) int {
	switch a.Kind {
	case ActRemoveReg, ActDrain, ActForceDelete:
		return 0
	case ActCreate:
		return 1
	case ActSSHAct:
		return 2
	default:
		return 3
	}
}

// buildViews 组装机器视图并维护 IdleSince/BusySince/OfflineSamples/FirstOnlineAt。
func buildViews(p Params, st *RepoState, obs observe.Obs) map[string]*machineView {
	views := map[string]*machineView{}
	for _, d := range obs.Droplets {
		m := &machineView{name: d.Name, droplet: d}
		for _, r := range obs.Runners {
			if p.OwnedRunnerRe != nil && !p.OwnedRunnerRe.MatchString(r.Name) {
				continue
			}
			if !runnerBelongsTo(r.Name, d.Name, p.RunnersPerDroplet) {
				continue
			}
			m.runners = append(m.runners, r)
		}
		views[d.Name] = m
	}
	for name, m := range views {
		ms := st.Machines[name]
		for _, r := range m.runners {
			if r.Status != "online" {
				continue
			}
			if ms.FirstOnlineAt.IsZero() {
				ms.FirstOnlineAt = obs.Now
				// 首次 online：清冷启失败计数；half-open 试探成功 → closed（§3.6.5）
				st.Breaker.Fails = 0
				if st.Breaker.State == "half-open" {
					st.Breaker.State = "closed"
				}
			}
			m.online++
			if !r.Busy {
				m.idle++
			}
		}
		switch {
		case m.online == 0:
			ms.OfflineSamples++
			ms.IdleSince, ms.BusySince = time.Time{}, time.Time{}
		case !m.anyBusy():
			if ms.IdleSince.IsZero() {
				ms.IdleSince = obs.Now
			}
			ms.BusySince = time.Time{}
			ms.OfflineSamples = 0
		default:
			if ms.BusySince.IsZero() {
				ms.BusySince = obs.Now
			}
			ms.IdleSince = time.Time{}
			ms.OfflineSamples = 0
		}
		st.Machines[name] = ms
	}
	return views
}

// demand 返回 Q（Matchable queued job 数）与 QAge（最老者年龄）。
func demand(obs observe.Obs) (int, time.Duration) {
	q, age := 0, time.Duration(0)
	for _, j := range obs.Jobs {
		if j.Status != "queued" {
			continue
		}
		q++
		if a := obs.Now.Sub(j.CreatedAt); a > age {
			age = a
		}
	}
	return q, age
}

// supply 计算 I（所辖 online 空闲 runner 数）、P（冷启在途机器数×rpd）、N。
// 冷启在途 = droplet 存在、runner 从未 online、未超时、未判死。
// 机器年龄以 droplet CreatedAt 为准（守护重启不丢，§3.6.1）。
func supply(p Params, st RepoState, views map[string]*machineView, now time.Time) (I, P, N int) {
	for name, m := range views {
		N++
		ms := st.Machines[name]
		if ms.Bad {
			continue
		}
		I += m.idle
		if ms.FirstOnlineAt.IsZero() && now.Sub(m.droplet.CreatedAt) <= p.ColdStartTimeout {
			P += p.RunnersPerDroplet
		}
	}
	return I, P, N
}

func recordActivity(st *RepoState, obs observe.Obs) {
	for _, j := range obs.Jobs {
		if j.Status == "in_progress" && !j.StartedAt.IsZero() {
			st.LastActivity = append(st.LastActivity, j.StartedAt)
		}
	}
	sort.Slice(st.LastActivity, func(i, j int) bool { return st.LastActivity[i].Before(st.LastActivity[j]) })
	out := st.LastActivity[:0]
	var last time.Time
	for _, t := range st.LastActivity {
		if t.Equal(last) {
			continue
		}
		last = t
		if obs.Now.Sub(t) <= lastActivityWindow {
			out = append(out, t)
		}
	}
	if len(out) > lastActivityKeep {
		out = out[len(out)-lastActivityKeep:]
	}
	st.LastActivity = out
}

func syncMachines(st *RepoState, obs observe.Obs) {
	seen := map[string]bool{}
	for _, d := range obs.Droplets {
		seen[d.Name] = true
		if _, ok := st.Machines[d.Name]; !ok {
			st.Machines[d.Name] = MachineState{FirstSeen: obs.Now}
		}
	}
	for name := range st.Machines {
		if !seen[name] {
			delete(st.Machines, name)
		}
	}
}

// runnerBelongsTo 判定 runner 名是否属于 droplet（rpd=1 同名；rpd=2 去掉
// -a/-b 后缀再对，§3.6.4）。
func runnerBelongsTo(runnerName, dropletName string, rpd int) bool {
	if runnerName == dropletName {
		return true
	}
	if rpd == 2 && strings.HasPrefix(runnerName, dropletName) {
		suffix := runnerName[len(dropletName):]
		if suffix == "-a" || suffix == "-b" {
			return true
		}
	}
	return false
}

// pruneEvents 只留窗口内的事件时刻。
func pruneEvents(events []time.Time, now time.Time, window time.Duration) []time.Time {
	out := events[:0]
	for _, t := range events {
		if now.Sub(t) <= window {
			out = append(out, t)
		}
	}
	return out
}
