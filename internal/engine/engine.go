// Package engine 把 observe → decide → budget → 执行粘起来（§3.8/§3.9）。
// 动作互相隔离：一个失败只记日志并回填状态（退避/事故证据），不影响同轮
// 其他动作；执行顺序 RemoveReg/Drain/ForceDelete → Create → SSHAct → Incident
// （decide 已按此序输出，engine 按序执行）。
// 机器来源经 machine.Provider（实施计划 §3.5）：建机/删机/孤儿扫描按机器的
// Pool 路由到对应池；SSH 用 Machine.Addr；命名由池的 Namer 决定。
package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/hoshinojian/ciorch/internal/budget"
	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/githubapp"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/observe"
)

// PublicTag 是编排器所辖机器的公共归属标记（§3.5）；编排器自身用
// ciorch-host（不匹配公共标记，不计入全局配额）。
const PublicTag = "ciorch"

// GHFace 是 engine 需要的 GitHub 侧写能力（*githubapp.Client 直接实现）。
type GHFace interface {
	MintToken(ctx context.Context, repo string) (string, error)
	RemoveRunner(ctx context.Context, repo string, runnerID int64) error
	ListRunners(ctx context.Context, repo string) ([]githubapp.Runner, error)
}

// PoolOf 返回某仓所用池的 Provider（cmd 装配；profile→池的解析属装配层）。
// 与 observe.PoolOf 同型（别名），装配层一份解析两处共用。
type PoolOf = observe.PoolOf

// SSHFace 经系统 ssh 子进程调机器上的 cirunner act（§3.8）。
type SSHFace interface {
	Act(ctx context.Context, ip, action string) ([]byte, error)
}

// CloudRenderer 渲染 cloud-init（真实现来自 cirunner 仓 pkg/cloudinit，阶段 3
// 机器侧重写后接入；此前 dry-run/测试用 fake）。
type CloudRenderer interface {
	Render(o RenderParams) ([]byte, error)
}

// RenderParams 是渲染入参的 engine 视图。
type RenderParams struct {
	RepoURL, ServiceRepo, RunnerName string
	Labels                           []string
	Token                            string
	PublicKeys                       []string
}

// Engine 是执行面。
type Engine struct {
	Cfg      *config.Config
	GH       GHFace
	SSH      SSHFace
	Renderer CloudRenderer
	Log      *log.Logger

	// Providers 是全部池（池名 → Provider）：删机按机器 Pool 路由、
	// 孤儿扫描覆盖所有池、Gate 全局计数按池聚合。
	Providers map[string]machine.Provider
	// PoolOf 返回仓所用池（建机/命名/观察）。必须注入。
	PoolOf PoolOf

	// Collector 由 daemon 注入（plan/status 直接观察也走它）；接口化便于测试。
	Collector Observer

	// States 是各仓决策记忆（daemon 持有并持久化）。并发安全：每仓一个
	// goroutine 并发 Reconcile（复审修正 7），读写与快照全部在 mu 内。
	States map[string]decide.RepoState
	// OnIncident 是事故回调（阶段 5 接 incident 模块；nil = 仅记日志）。
	OnIncident func(ctx context.Context, repo string, a decide.Action)
	// Gate 是全局建机闸门（配额+预算互斥判定，复审修正 8；nil = 不设限，
	// 仅测试用——daemon 必须注入）。
	Gate *budget.Gate

	mu sync.Mutex
}

// New 构造。
func New(cfg *config.Config, gh GHFace, providers map[string]machine.Provider, poolOf PoolOf, ssh SSHFace, r CloudRenderer, lg *log.Logger) *Engine {
	if lg == nil {
		lg = log.New(io.Discard, "", 0)
	}
	return &Engine{
		Cfg: cfg, GH: gh, Providers: providers, PoolOf: poolOf, SSH: ssh, Renderer: r, Log: lg,
		States: map[string]decide.RepoState{},
	}
}

// Observer 是一轮观察能力（observe.Collector 实现，测试可 fake）。
type Observer interface {
	Collect(ctx context.Context, r config.RepoEntry, p config.Profile) (observe.Obs, error)
}

// SetCollector 注入观察器。
func (e *Engine) SetCollector(c Observer) { e.Collector = c }

// RoundStats 是一轮的量化摘要（§3.9 摘要行与 daemon 轮询切换的数据源）。
type RoundStats struct {
	Q, N     int
	QAge     time.Duration
	I, P     int
	Complete bool
}

// Reconcile 对一个仓走一轮：observe → decide → 执行。
// dryRun = 只观察决策写日志不执行。建机配额/预算经 Gate 互斥判定（复审修正 8）。
func (e *Engine) Reconcile(ctx context.Context, repo config.RepoEntry, dryRun bool) ([]decide.Action, RoundStats, error) {
	if e.Collector == nil {
		return nil, RoundStats{}, fmt.Errorf("collector 未注入")
	}
	p := e.Cfg.ProfileOf(repo)
	pool := e.PoolOf(repo, p)
	if pool == nil {
		return nil, RoundStats{}, fmt.Errorf("仓 %s 的池未装配（PoolOf 未注入或池缺失）", repo.Repo)
	}
	obs, err := e.Collector.Collect(ctx, repo, p)
	if err != nil {
		e.Log.Printf("[%s] 观察不完整: %v", repo.Repo, err)
	}
	params := decide.Params{
		MaxRunners:        repo.MaxRunners,
		RunnersPerDroplet: p.RunnersPerDroplet,
		// 命名空间由池的 Namer 给出（vpsctl：{prefix}-{account}-{region}-NN）
		OwnedRunnerRe: regexp.MustCompile(pool.Namer().Pattern(repo.NamePrefix())),
	}
	e.mu.Lock()
	state := e.States[repo.Repo]
	e.mu.Unlock()
	acts, st := decide.Decide(params, state, obs)
	e.mu.Lock()
	e.States[repo.Repo] = st
	e.mu.Unlock()
	stats := e.stats(repo, obs, params)
	if dryRun {
		for _, a := range acts {
			e.Log.Printf("[%s] dry-run %s: %s", repo.Repo, a.Kind, a.Reason)
		}
		return acts, stats, nil
	}
	e.execute(ctx, repo, obs, acts)
	return acts, stats, nil
}

// stats 从观察与状态算 Q/QAge/I/P/N（§3.6.2 口径；冷启窗口与 decide 同源，
// 此前写死 8 分钟会随参数覆盖漂移）。
func (e *Engine) stats(repo config.RepoEntry, obs observe.Obs, params decide.Params) RoundStats {
	p := e.Cfg.ProfileOf(repo)
	st := RoundStats{N: len(obs.Machines), Complete: obs.Complete}
	re := regexp.MustCompile(e.PoolOf(repo, p).Namer().Pattern(repo.NamePrefix()))
	for _, j := range obs.Jobs {
		if j.Status == "queued" {
			st.Q++
			if a := obs.Now.Sub(j.CreatedAt); a > st.QAge {
				st.QAge = a
			}
		}
	}
	machineNames := map[string]bool{}
	for _, m := range obs.Machines {
		machineNames[m.Name] = true
	}
	e.mu.Lock()
	state := e.States[repo.Repo]
	e.mu.Unlock()
	for _, r := range obs.Runners {
		if r.Status == "online" && !r.Busy && re.MatchString(r.Name) {
			st.I++
		}
	}
	for name := range machineNames {
		ms := state.Machines[name]
		if ms.FirstOnlineAt.IsZero() && !ms.Bad && obs.Now.Sub(findMachine(obs, name).CreatedAt) <= params.ColdStartTimeout {
			st.P += p.RunnersPerDroplet
		}
	}
	return st
}

func findMachine(obs observe.Obs, name string) machine.Machine {
	for _, m := range obs.Machines {
		if m.Name == name {
			return m
		}
	}
	return machine.Machine{}
}

// StatesSnapshot 返回决策记忆的深拷贝（持久化/查询用，锁内拷贝——复审修正 7）。
func (e *Engine) StatesSnapshot() map[string]decide.RepoState {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]decide.RepoState, len(e.States))
	for k, v := range e.States {
		machines := make(map[string]decide.MachineState, len(v.Machines))
		for mk, mv := range v.Machines {
			machines[mk] = mv
		}
		v.Machines = machines
		out[k] = v
	}
	return out
}

// execute 按序执行动作；动作隔离（§3.8）：失败记日志/回填，不中断。
func (e *Engine) execute(ctx context.Context, repo config.RepoEntry, obs observe.Obs, acts []decide.Action) {
	for _, a := range acts {
		switch a.Kind {
		case decide.ActCreate:
			// 全局配额+预算占坑（复审修正 8：Reserve→建机→Commit/Abort，
			// 判定与建机之间的间隙不会让两仓抢掉同一个名额）
			if e.Gate != nil {
				res, ok, reason := e.Gate.Reserve(ctx, liveHourlyCost(obs))
				if !ok {
					qAge := queueAge(obs)
					e.Log.Printf("[%s] 跳过建机（场景 F）：%s", repo.Repo, reason)
					if qAge >= 15*time.Minute {
						e.fireIncident(ctx, repo, decide.Action{Kind: decide.ActIncident, IncidentType: "F",
							Reason: "配额/预算不足且排队超 15 分钟：" + reason})
					}
					continue
				}
				created, err := e.create(ctx, repo, obs)
				if err != nil {
					e.Gate.Abort(res)
					e.Log.Printf("[%s] 建机失败: %v", repo.Repo, err)
					e.mu.Lock()
					st := e.States[repo.Repo]
					e.mu.Unlock()
					if inc, fire := decide.RecordCreateResult(decide.Params{}, &st, time.Now(), err); fire {
						e.fireIncident(ctx, repo, inc)
					}
					e.mu.Lock()
					e.States[repo.Repo] = st
					e.mu.Unlock()
					continue
				}
				e.Gate.Commit(res, created) // 占坑保留到列表可见/TTL
				continue
			}
			if _, err := e.create(ctx, repo, obs); err != nil {
				e.Log.Printf("[%s] 建机失败: %v", repo.Repo, err)
				e.mu.Lock()
				st := e.States[repo.Repo]
				e.mu.Unlock()
				if inc, fire := decide.RecordCreateResult(decide.Params{}, &st, time.Now(), err); fire {
					e.fireIncident(ctx, repo, inc)
				}
				e.mu.Lock()
				e.States[repo.Repo] = st
				e.mu.Unlock()
			}
		case decide.ActDrain:
			// 先注销注册，成功后才删机（防 #7 竞态；注销失败本轮放弃删机——
			// V1 未实测前保守）。RunnerID==0（注册已不在，机器命名空间内查无
			// 注册）则跳过注销直接删机，否则会永远注销失败卡死删机。
			if a.RunnerID != 0 {
				if err := e.GH.RemoveRunner(ctx, repo.Repo, a.RunnerID); err != nil {
					e.Log.Printf("[%s] Drain 注销失败，保留机器下轮重判: %v", repo.Repo, err)
					continue
				}
			}
			// rpd=2：顺带清 -a/-b 实例的注册（按名清理，尽力而为）
			e.removeRunnerByName(ctx, repo, a.DropletName)
			if err := e.deleteMachine(ctx, obs, a.DropletID); err != nil {
				e.Log.Printf("[%s] Drain 删机失败: %v", repo.Repo, err)
				continue
			}
			e.recordDeletion(obs, a.DropletID)
			e.Log.Printf("[%s] 已回收 %s（%s）", repo.Repo, a.DropletName, a.Reason)
		case decide.ActForceDelete:
			// 强删：先删机器再清注册（job 已判死/注册从未成功）
			if err := e.deleteMachine(ctx, obs, a.DropletID); err != nil {
				e.Log.Printf("[%s] 强删失败: %v", repo.Repo, err)
				continue
			}
			e.recordDeletion(obs, a.DropletID)
			if a.RunnerID != 0 {
				_ = e.GH.RemoveRunner(ctx, repo.Repo, a.RunnerID)
			} else if a.RunnerName != "" {
				e.removeRunnerByName(ctx, repo, a.RunnerName)
			}
			e.Log.Printf("[%s] 已强删 %s（%s）", repo.Repo, a.DropletName, a.Reason)
		case decide.ActRemoveReg:
			if err := e.GH.RemoveRunner(ctx, repo.Repo, a.RunnerID); err != nil {
				e.Log.Printf("[%s] 清注册失败: %v", repo.Repo, err)
			}
		case decide.ActSSHAct:
			addr := machineAddr(obs, a.DropletName)
			if addr == "" {
				e.Log.Printf("[%s] SSHAct 跳过：%s 无地址（观察里查不到）", repo.Repo, a.DropletName)
				continue
			}
			if _, err := e.SSH.Act(ctx, addr, a.SSHAct); err != nil {
				e.Log.Printf("[%s] SSHAct %s 失败: %v", repo.Repo, a.SSHAct, err)
			}
		case decide.ActIncident:
			e.fireIncident(ctx, repo, a)
		}
	}
}

// deleteMachine 按机器的 Pool 路由到对应池删机。
func (e *Engine) deleteMachine(ctx context.Context, obs observe.Obs, id string) error {
	pool, m := findMachineByID(obs, id)
	if pool == "" {
		return fmt.Errorf("机器 %s 不在本轮观察里，无法路由到池", id)
	}
	return e.Providers[pool].Delete(ctx, m.ID)
}

// removeRunnerByName 按名清理注册（ForceDelete 时 id 缺失的兜底；顺带清
// rpd=2 的 -a/-b 实例）。
func (e *Engine) removeRunnerByName(ctx context.Context, repo config.RepoEntry, name string) {
	runners, err := e.GH.ListRunners(ctx, repo.Repo)
	if err != nil {
		e.Log.Printf("[%s] 按名清注册失败（列 runner）: %v", repo.Repo, err)
		return
	}
	for _, r := range runners {
		if r.Name == name || r.Name == name+"-a" || r.Name == name+"-b" {
			if err := e.GH.RemoveRunner(ctx, repo.Repo, r.ID); err != nil {
				e.Log.Printf("[%s] 清注册 %s 失败: %v", repo.Repo, r.Name, err)
			}
		}
	}
}

func (e *Engine) fireIncident(ctx context.Context, repo config.RepoEntry, a decide.Action) {
	e.Log.Printf("[%s] 事故 %s/%s: %s", repo.Repo, a.IncidentType, a.IncidentKey, a.Reason)
	if e.OnIncident != nil {
		e.OnIncident(ctx, repo.Repo, a)
	}
}

// create：铸 token → 渲染 cloud-init → 池 Provider 建机（名字由池的 Namer
// 取、cloud-init 落盘/清理由 Provider 负责）。返回机器名（Gate 占坑沉淀用）。
func (e *Engine) create(ctx context.Context, repo config.RepoEntry, obs observe.Obs) (string, error) {
	p := e.Cfg.ProfileOf(repo)
	prov := e.PoolOf(repo, p)
	name := prov.Namer().Next(repo.NamePrefix(), machineNames(obs.Machines))
	ctxTO, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if e.Renderer == nil {
		return "", fmt.Errorf("cloud-init 渲染器未接入（cirunner pkg/cloudinit，阶段 3）——本阶段只允许 dry-run")
	}
	token, err := e.GH.MintToken(ctxTO, repo.Repo)
	if err != nil {
		return "", fmt.Errorf("铸 registration token: %w", err)
	}
	ci, err := e.Renderer.Render(RenderParams{
		RepoURL:     "https://github.com/" + repo.Repo,
		ServiceRepo: serviceRepo(repo.Repo),
		RunnerName:  name,
		Labels:      p.Labels,
		Token:       token,
		PublicKeys:  e.publicKeys(),
	})
	if err != nil {
		return "", fmt.Errorf("渲染 cloud-init: %w", err)
	}
	m, err := prov.Create(ctxTO, machine.CreateRequest{
		Name:     name,
		Tags:     []string{PublicTag, repo.Tag()},
		UserData: ci,
	})
	if err != nil {
		return "", err // user-data 留存由 Provider 负责（失败保留供诊断）
	}
	e.Log.Printf("[%s] 已建机 %s (id=%s, addr=%q)", repo.Repo, m.Name, m.ID, m.Addr)
	return m.Name, nil
}

func (e *Engine) publicKeys() []string {
	var keys []string
	for _, f := range e.Cfg.SSHPublicKeyFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			e.Log.Printf("读公钥 %s 失败: %v", f, err)
			continue
		}
		keys = append(keys, string(raw))
	}
	return keys
}

// ScanOrphans 场景 J：对每个池按公共标记列机，机器不带任何已配置仓标记
// → 只告警**绝不删**（等人确认）。
func (e *Engine) ScanOrphans(ctx context.Context) ([]machine.Machine, error) {
	repoTags := map[string]bool{}
	for _, r := range e.Cfg.Repos {
		repoTags[r.Tag()] = true
	}
	pools := make([]string, 0, len(e.Providers))
	for name := range e.Providers {
		pools = append(pools, name)
	}
	sort.Strings(pools)
	seen := map[string]bool{}
	var orphans []machine.Machine
	for _, pool := range pools {
		ms, err := e.Providers[pool].List(ctx, PublicTag)
		if err != nil {
			return nil, fmt.Errorf("池 %s 列机: %w", pool, err)
		}
		for _, m := range ms {
			key := pool + "\x00" + m.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			if !hasAnyTag(m.Tags, repoTags) {
				orphans = append(orphans, m)
			}
		}
	}
	return orphans, nil
}

func hasAnyTag(tags []string, want map[string]bool) bool {
	for _, t := range tags {
		if want[t] {
			return true
		}
	}
	return false
}

// queueAge 取观察里最老 queued job 年龄（场景 F 升级判据）。
func queueAge(obs observe.Obs) time.Duration {
	var age time.Duration
	for _, j := range obs.Jobs {
		if j.Status == "queued" {
			if a := obs.Now.Sub(j.CreatedAt); a > age {
				age = a
			}
		}
	}
	return age
}

// recordDeletion 删机成功后入账当日花费（复审修正 8：budget.RecordDeletion
// 此前无人调用，每日预算完全不生效）。
func (e *Engine) recordDeletion(obs observe.Obs, machineID string) {
	if e.Gate == nil {
		return
	}
	for _, m := range obs.Machines {
		if m.ID == machineID {
			e.Gate.RecordDeletion(obs.Now, m.HourlyCost, m.CreatedAt)
			return
		}
	}
}

// liveHourlyCost 取本仓现存机器每小时成本作新机近似价（同 profile；
// 无机器=0，Gate 侧用全局任一在册价兜底）。
func liveHourlyCost(obs observe.Obs) float64 {
	for _, m := range obs.Machines {
		return m.HourlyCost
	}
	return 0
}

func machineAddr(obs observe.Obs, name string) string {
	for _, m := range obs.Machines {
		if m.Name == name {
			return m.Addr
		}
	}
	return ""
}

func machineNames(ms []machine.Machine) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// findMachineByID 在观察里找机器，返回其所属池名（路由删机用）。
func findMachineByID(obs observe.Obs, id string) (string, machine.Machine) {
	for _, m := range obs.Machines {
		if m.ID == id {
			return m.Pool, m
		}
	}
	return "", machine.Machine{}
}

// serviceRepo 把 owner/name 转成 systemd unit 用的连字符形式（坑 4：点号会
// Unit not found）。
func serviceRepo(repo string) string {
	out := make([]byte, 0, len(repo))
	for i := 0; i < len(repo); i++ {
		c := repo[i]
		if c == '/' || c == '.' {
			c = '-'
		}
		out = append(out, c)
	}
	return string(out)
}
