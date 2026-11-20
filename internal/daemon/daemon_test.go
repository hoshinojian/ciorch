package daemon

import (
	"io"
	"log"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/budget"
	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/engine"
)

func testDaemon(t *testing.T) *Daemon {
	t.Helper()
	cfg := &config.Config{
		StateDir: t.TempDir(),
		Global:   config.GlobalCfg{MaxDroplets: 2, DailyBudgetUSD: 5},
		Profiles: map[string]config.Profile{
			"go-heavy": {Account: "team2-ci", Region: "sgp1", RunnersPerDroplet: 1},
		},
		Repos: []config.RepoEntry{{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}},
	}
	// 本文件只测间隔切换/触发合并/摘要，不触 IO 面——接口传 nil。
	e := engine.New(cfg, nil, nil, nil, nil, log.New(io.Discard, "", 0))
	return New(cfg, e, &budget.Budget{MaxDroplets: 2, DailyBudgetUSD: 5}, nil, log.New(io.Discard, "", 0))
}

// 间隔切换（§3.6.6）：空闲 → poll_idle；冷启在途/在忙/补救中 → poll_active。
func TestNextIntervalSwitching(t *testing.T) {
	d := testDaemon(t)
	d.PollActive, d.PollIdle = 15*time.Second, 60*time.Second
	repo := d.Cfg.Repos[0]

	if got := d.nextInterval(repo, time.Now()); got != 60*time.Second {
		t.Fatalf("无状态应空闲档，got %v", got)
	}
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstSeen: time.Now()}, // 冷启在途（FirstOnlineAt 零值）
	}}
	if got := d.nextInterval(repo, time.Now()); got != 15*time.Second {
		t.Fatalf("冷启在途应活跃档，got %v", got)
	}
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstOnlineAt: time.Now(), BusySince: time.Now()},
	}}
	if got := d.nextInterval(repo, time.Now()); got != 15*time.Second {
		t.Fatalf("busy 应活跃档，got %v", got)
	}
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstOnlineAt: time.Now(), Remedy: "restarting", RemedyAt: time.Now()},
	}}
	if got := d.nextInterval(repo, time.Now()); got != 15*time.Second {
		t.Fatalf("补救中应活跃档，got %v", got)
	}
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstOnlineAt: time.Now(), IdleSince: time.Now()},
	}}
	if got := d.nextInterval(repo, time.Now()); got != 60*time.Second {
		t.Fatalf("在线空闲应空闲档，got %v", got)
	}
}

// Trigger 合并：channel 满时再触发不阻塞。
func TestTriggerNonBlocking(t *testing.T) {
	d := testDaemon(t)
	ch := make(chan struct{}, 1)
	repo := d.Cfg.Repos[0].Repo
	d.mu.Lock()
	d.triggers[repo] = ch
	d.mu.Unlock()
	d.Trigger(repo)
	d.Trigger(repo) // 第二次应合并而非阻塞
	select {
	case <-ch:
	default:
		t.Fatal("应有一次触发入队")
	}
}

// summary：无动作时输出 active 标记；有动作时列动作名。
func TestSummary(t *testing.T) {
	d := testDaemon(t)
	repo := d.Cfg.Repos[0]
	d.Engine.States[repo.Repo] = decide.RepoState{Machines: map[string]decide.MachineState{
		"m1": {FirstSeen: time.Now()},
	}}
	if s := d.summary(repo, nil); s != "actions=[] active=true" {
		t.Fatalf("summary 不对: %q", s)
	}
	if s := d.summary(repo, []decide.Action{{Kind: decide.ActDrain}, {Kind: decide.ActCreate}}); s != "actions=[drain,create]" {
		t.Fatalf("动作摘要不对: %q", s)
	}
}
