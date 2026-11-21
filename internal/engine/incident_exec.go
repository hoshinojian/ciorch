// incident.Executor 的 engine 实现（REDESIGN §3.10.5 白名单动作）。
// 动作仍受 §3.7 配额/预算约束：ReplaceMachine 无配额时降级为 drain 后再建。
// 另有 pause/pool 覆写两个状态开关（execute 建机前检查），供 pause_repo /
// switch_pool 动作与人工清理（HumanCleared）使用。
package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/observe"
)

// GHFace 在此扩展：rerun_failed_jobs 动作需要（*githubapp.Client 已实现）。
// 见 engine.go 的接口定义（本注释仅说明依赖来源）。

// HumanCleared 人工关闭 issue 后：清熔断、解除 pause/池覆写、CreateFailStreak=0。
func (e *Engine) HumanCleared(repo string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.States[repo]
	decide.BreakerReset(&st)
	st.CreateFailStreak = 0
	st.CreateBackoffUntil = time.Time{}
	e.States[repo] = st
	delete(e.pausedUntil, repo)
	delete(e.poolOverride, repo)
}

// RestartRunner 经 ssh 重启机器上的 runner 服务（restart-runner）。
func (e *Engine) RestartRunner(ctx context.Context, repo, machineName string) error {
	addr := machineAddr(e.lastObsOf(repo), machineName)
	if addr == "" {
		return fmt.Errorf("机器 %s 无地址（无观察快照或 IP 未分配）", machineName)
	}
	if _, err := e.SSH.Act(ctx, addr, "restart-runner"); err != nil {
		return err
	}
	return nil
}

// DrainMachine 注销该机器的全部注册并删机（按机器 Pool 路由）。
func (e *Engine) DrainMachine(ctx context.Context, repo, machineName string) error {
	obs := e.lastObsOf(repo)
	var target *machine.Machine
	for i, m := range obs.Machines {
		if m.Name == machineName {
			target = &obs.Machines[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("机器 %s 不在观察快照里", machineName)
	}
	e.removeRunnerByName(ctx, config.RepoEntry{Repo: repo}, machineName)
	pool := e.Providers[target.Pool]
	if pool == nil {
		return fmt.Errorf("机器 %s 的池 %s 未装配", machineName, target.Pool)
	}
	if err := pool.Delete(ctx, target.ID); err != nil {
		return err
	}
	if e.Gate != nil {
		e.Gate.RecordDeletion(obs.Now, target.HourlyCost, target.CreatedAt)
	}
	return nil
}

// ReplaceMachine 替换机器：先试建新机（配额/预算闸门）；无配额则先回收旧机
// 再建（§3.10.5 降级路径）。
func (e *Engine) ReplaceMachine(ctx context.Context, repo, machineName string) error {
	entry := e.repoEntry(repo)
	p := e.Cfg.ProfileOf(entry)
	pool := e.poolFor(entry, p)
	tryCreate := func() error {
		if e.Gate != nil {
			res, ok, reason := e.Gate.Reserve(ctx, pool.Name)
			if !ok {
				return fmt.Errorf("配额/预算：%s", reason)
			}
			created, err := e.create(ctx, entry, e.lastObsOf(repo), nil)
			if err != nil {
				e.Gate.Abort(res)
				return err
			}
			e.Gate.Commit(res, created)
			return nil
		}
		_, err := e.create(ctx, entry, e.lastObsOf(repo), nil)
		return err
	}
	if err := tryCreate(); err == nil {
		return nil // 先建后拆成功
	} else {
		e.Log.Printf("[%s] 替换机无配额/失败，降级先回收旧机再建：%v", repo, err)
	}
	// 降级：drain 旧机（释放配额）→ 再建
	if err := e.DrainMachine(ctx, repo, machineName); err != nil {
		return fmt.Errorf("降级回收旧机失败: %w", err)
	}
	return tryCreate()
}

// PauseRepo 该仓停止建机 d 分钟（已有机器照常跑完与回收；只拦 ActCreate）。
func (e *Engine) PauseRepo(repo string, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pausedUntil[repo] = e.now().Add(d)
}

// ClearBreaker 清该仓建机熔断（CLI reset / 事故动作共用）。
func (e *Engine) ClearBreaker(repo string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.States[repo]
	decide.BreakerReset(&st)
	e.States[repo] = st
}

// SwitchPool 临时改用指定池建机（switch_pool；单池配置下校验层已挡，
// 多池调度落地后此处生效）。
func (e *Engine) SwitchPool(repo, pool string, d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.poolOverride[repo] = poolOverrideEntry{Pool: pool, Until: e.now().Add(d)}
}

// RerunFailedJobs 重跑 run 的失败 job（githubapp.Client 实现 GHFace 的该方法）。
func (e *Engine) RerunFailedJobs(ctx context.Context, repo string, runID int64) error {
	return e.GH.RerunFailedJobs(ctx, repo, runID)
}

// ---------- 状态开关与观察快照（engine 主体使用） ----------

type poolOverrideEntry struct {
	Pool  string
	Until time.Time
}

// poolFor 解析仓当前生效的池：switch_pool 覆写优先（未过期且池存在），
// 否则装配层 PoolOf。
func (e *Engine) poolFor(r config.RepoEntry, p config.Profile) observe.Pool {
	e.mu.Lock()
	ov, ok := e.poolOverride[r.Repo]
	e.mu.Unlock()
	if ok && e.now().Before(ov.Until) {
		if prov, exists := e.Providers[ov.Pool]; exists {
			return observe.Pool{Name: ov.Pool, Provider: prov}
		}
	}
	return e.PoolOf(r, p)
}

// paused 判断仓是否处于建机暂停。
func (e *Engine) paused(repo string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.pausedUntil[repo]
	return ok && e.now().Before(until)
}

// LastObsOf 返回该仓最近一轮观察快照（事故动作/证据用；无则零值）。
func (e *Engine) LastObsOf(repo string) observe.Obs {
	return e.lastObsOf(repo)
}

// lastObsOf 返回该仓最近一轮观察快照（内部）。
func (e *Engine) lastObsOf(repo string) observe.Obs {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastObs[repo]
}

// noteObs 记录最近观察（Reconcile 每轮调用）。
func (e *Engine) noteObs(repo string, obs observe.Obs) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastObs[repo] = obs
}

// repoEntry 按仓名查配置条目（事故动作只有 repo 字符串）。
func (e *Engine) repoEntry(repo string) config.RepoEntry {
	for _, r := range e.Cfg.Repos {
		if r.Repo == repo {
			return r
		}
	}
	return config.RepoEntry{Repo: repo}
}

// now 可注入时钟（测试；默认 time.Now）。
func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
