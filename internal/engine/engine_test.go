package engine

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/observe"
)

var tnow = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// ---------- fakes ----------

type fakeObs struct{ obs observe.Obs }

func (f *fakeObs) Collect(ctx context.Context, r config.RepoEntry, p config.Profile) (observe.Obs, error) {
	return f.obs, nil
}

type fakeGH struct {
	token       string
	removeErr   map[int64]error
	removed     []int64
	listRunners []observe.Runner
}

func (f *fakeGH) MintToken(ctx context.Context, repo string) (string, error) {
	if f.token == "" {
		return "REGTOKEN1234567890", nil
	}
	return f.token, nil
}
func (f *fakeGH) RemoveRunner(ctx context.Context, repo string, id int64) error {
	if f.removeErr != nil {
		if err, ok := f.removeErr[id]; ok {
			return err
		}
	}
	f.removed = append(f.removed, id)
	return nil
}
func (f *fakeGH) ListRunners(ctx context.Context, repo string) ([]observe.Runner, error) {
	return f.listRunners, nil
}

type fakeVPS struct {
	created   []CreateOpts
	deleted   []string
	order     []string
	createErr error
	droplets  []Droplet // ListByTag 结果
}

func (f *fakeVPS) Create(ctx context.Context, o CreateOpts) (CreatedDroplet, error) {
	if f.createErr != nil {
		return CreatedDroplet{}, f.createErr
	}
	f.created = append(f.created, o)
	f.order = append(f.order, "create")
	return CreatedDroplet{Account: o.Account, ID: fmt.Sprintf("id%d", len(f.created)), Name: "ci-nms-team2-ci-sgp1-01", IPv4Public: ""}, nil
}
func (f *fakeVPS) Delete(ctx context.Context, account, id string) error {
	f.deleted = append(f.deleted, id)
	f.order = append(f.order, "delete:"+id)
	return nil
}
func (f *fakeVPS) ListByTag(ctx context.Context, tag, account string) ([]Droplet, error) {
	// 贴近 vpsctl 契约：按 tag 精确过滤
	var out []Droplet
	for _, d := range f.droplets {
		for _, tg := range d.Tags {
			if tg == tag {
				out = append(out, d)
				break
			}
		}
	}
	return out, nil
}

type fakeSSH struct{ acts []string }

func (f *fakeSSH) Act(ctx context.Context, ip, action string) ([]byte, error) {
	f.acts = append(f.acts, ip+":"+action)
	return []byte(`{"ok":true}`), nil
}

type fakeRenderer struct{}

func (fakeRenderer) Render(o RenderParams) ([]byte, error) {
	if !strings.Contains(o.RepoURL, "github.com") || o.Token == "" || o.RunnerName == "" {
		return nil, fmt.Errorf("渲染参数不全: %+v", o)
	}
	return []byte("#cloud-config\n"), nil
}

// ---------- fixtures ----------

func testCfg(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		StateDir:          t.TempDir(),
		Global:            config.GlobalCfg{MaxDroplets: 2, DailyBudgetUSD: 5},
		SSHPublicKeyFiles: []string{},
		Profiles: map[string]config.Profile{
			"go-heavy": {Account: "team2-ci", Region: "sgp1", Size: "c2-4vcpu-8gb", Image: "ubuntu-24-04-x64", Labels: []string{"ci"}, RunnersPerDroplet: 1},
		},
		Repos: []config.RepoEntry{{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}},
	}
}

func newEngine(t *testing.T, gh *fakeGH, vps *fakeVPS, obsrv Observer) *Engine {
	t.Helper()
	e := New(testCfg(t), gh, vps, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.SetCollector(obsrv)
	return e
}

func nmsRepo() config.RepoEntry {
	return config.RepoEntry{Repo: "hoshinojian/nms", Profile: "go-heavy", MaxRunners: 2}
}

// ---------- §8.1 engine 条目 ----------

// 执行顺序：删在建前（硬寿命 Drain + 扩容 Create 同轮）。
func TestExecuteOrderDeleteBeforeCreate(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true}
	for i := 0; i < 5; i++ {
		o.Jobs = append(o.Jobs, observe.Job{ID: int64(i), RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)})
	}
	o.Droplets = []observe.Droplet{{ID: "d1", Name: name, CreatedAt: tnow.Add(-9 * time.Hour), PrivateIP: "10.0.0.1"}}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	gh := &fakeGH{}
	vps := &fakeVPS{}
	// 预置状态：机器早已 online 且空闲 30m（硬寿命 9h → Drain；Q=5 → Create）
	st := preloadState(gh, name)
	e := newEngine(t, gh, vps, &fakeObs{o})
	e.States[nmsRepo().Repo] = st
	budget := 2
	if _, err := e.Reconcile(context.Background(), nmsRepo(), false, &budget); err != nil {
		t.Fatal(err)
	}
	if len(vps.order) < 2 || !strings.HasPrefix(vps.order[0], "delete:") || vps.order[1] != "create" {
		t.Fatalf("删应在建前：order=%v", vps.order)
	}
}

// Drain：注销失败 → 本轮不删机。
func TestDrainRegistrationFailureKeepsMachine(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true}
	o.Droplets = []observe.Droplet{{ID: "d1", Name: name, CreatedAt: tnow.Add(-9 * time.Hour)}}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	gh := &fakeGH{removeErr: map[int64]error{42: fmt.Errorf("422 Cannot delete a runner that is busy")}}
	vps := &fakeVPS{}
	e := newEngine(t, gh, vps, &fakeObs{o})
	e.States[nmsRepo().Repo] = preloadState(gh, name)
	if _, err := e.Reconcile(context.Background(), nmsRepo(), false, nil); err != nil {
		t.Fatal(err)
	}
	if len(vps.deleted) != 0 {
		t.Fatalf("注销失败不应删机：deleted=%v", vps.deleted)
	}
}

// 动作隔离：Drain 注销失败，同轮 Create 照做。
func TestActionIsolation(t *testing.T) {
	name := "ci-nms-team2-ci-sgp1-01"
	o := observe.Obs{Now: tnow, Complete: true}
	for i := 0; i < 5; i++ {
		o.Jobs = append(o.Jobs, observe.Job{ID: int64(i), RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)})
	}
	o.Droplets = []observe.Droplet{{ID: "d1", Name: name, CreatedAt: tnow.Add(-9 * time.Hour), PrivateIP: "10.0.0.1"}}
	o.Runners = []observe.Runner{{ID: 42, Name: name, Status: "online", Busy: false}}
	gh := &fakeGH{removeErr: map[int64]error{42: fmt.Errorf("boom")}}
	vps := &fakeVPS{}
	e := newEngine(t, gh, vps, &fakeObs{o})
	e.States[nmsRepo().Repo] = preloadState(gh, name)
	budget := 2
	if _, err := e.Reconcile(context.Background(), nmsRepo(), false, &budget); err != nil {
		t.Fatal(err)
	}
	if len(vps.created) != 1 {
		t.Fatalf("一个动作失败不应拖垮同轮其他动作：created=%v", vps.created)
	}
}

// 建机失败保留 user-data（诊断用）；成功即删（含 1h token）。
func TestCreateUserDataLifecycle(t *testing.T) {
	o := observe.Obs{Now: tnow, Complete: true}
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}}
	cfg := testCfg(t)

	// 失败：文件保留
	gh := &fakeGH{}
	vps := &fakeVPS{createErr: fmt.Errorf("DO 503")}
	e := New(cfg, gh, vps, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e.SetCollector(&fakeObs{o})
	if _, err := e.Reconcile(context.Background(), nmsRepo(), false, nil); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(cfg.StateDir, "cloudinit", "*.yaml"))
	if len(files) != 1 {
		t.Fatalf("建机失败应保留 user-data：files=%v", files)
	}
	// 退避入账（场景 I）
	if e.States[nmsRepo().Repo].CreateFailStreak != 1 {
		t.Fatalf("失败应回填 CreateFailStreak=1，got %+v", e.States[nmsRepo().Repo])
	}

	// 成功：文件删除；空 IP 不是错误
	vps2 := &fakeVPS{}
	e2 := New(testCfg(t), &fakeGH{}, vps2, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	e2.SetCollector(&fakeObs{o})
	if _, err := e2.Reconcile(context.Background(), nmsRepo(), false, nil); err != nil {
		t.Fatal(err)
	}
	if len(vps2.created) != 1 {
		t.Fatalf("应建 1 台：%v", vps2.created)
	}
	if got := vps2.created[0]; !strings.Contains(strings.Join(got.Tags, ","), "ciorch") {
		t.Fatalf("建机 tags 应含公共 tag：%v", got.Tags)
	}
	files2, _ := filepath.Glob(filepath.Join(e2.stateDirOrDefault(), "cloudinit", "*.yaml"))
	if len(files2) != 0 {
		t.Fatalf("建机成功应删 user-data：%v", files2)
	}
	_ = os.Remove(filepath.Join(cfg.StateDir, "cloudinit", "ci-nms-team2-ci-sgp1-01.yaml"))
}

// dry-run：只决策不执行。
func TestDryRunExecutesNothing(t *testing.T) {
	o := observe.Obs{Now: tnow, Complete: true}
	o.Jobs = []observe.Job{{ID: 1, RunID: 1, Status: "queued", CreatedAt: tnow.Add(-time.Minute)}}
	gh := &fakeGH{}
	vps := &fakeVPS{}
	e := newEngine(t, gh, vps, &fakeObs{o})
	acts, err := e.Reconcile(context.Background(), nmsRepo(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) == 0 || len(vps.created) != 0 || len(gh.removed) != 0 {
		t.Fatalf("dry-run 不执行：acts=%v created=%v", acts, vps.created)
	}
}

// 场景 J：孤儿 droplet（公共 tag、无仓 tag）只报告，绝不删。
func TestOrphansReportedNeverDeleted(t *testing.T) {
	cfg := testCfg(t)
	repoTag := nmsRepo().Tag()
	vps := &fakeVPS{droplets: []Droplet{
		{Account: "team2-ci", ID: "1", Name: "mystery-01", Tags: []string{"ciorch"}},                       // 孤儿
		{Account: "team2-ci", ID: "2", Name: "ci-nms-team2-ci-sgp1-01", Tags: []string{"ciorch", repoTag}}, // 正常
		{Account: "team2-ci", ID: "3", Name: "ciorch-host-1", Tags: []string{"ciorch-host"}},               // 编排器自身（不列出来）
	}}
	e := New(cfg, &fakeGH{}, vps, &fakeSSH{}, fakeRenderer{}, log.New(io.Discard, "", 0))
	orphans, err := e.ScanOrphans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].ID != "1" {
		t.Fatalf("应只识别 1 台孤儿：%+v", orphans)
	}
	if len(vps.deleted) != 0 {
		t.Fatal("孤儿绝不删除")
	}
}

// preloadState 构造"机器已 online 且空闲超 30m"的状态（硬寿命/空闲判定用）。
func preloadState(gh *fakeGH, name string) decide.RepoState {
	return decide.RepoState{
		Machines: map[string]decide.MachineState{
			name: {FirstOnlineAt: tnow.Add(-9 * time.Hour), IdleSince: tnow.Add(-30 * time.Minute)},
		},
	}
}
