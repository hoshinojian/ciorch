package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoshinojian/ciorch/internal/config"
	"github.com/hoshinojian/ciorch/internal/decide"
	"github.com/hoshinojian/ciorch/internal/engine"
	"github.com/hoshinojian/ciorch/internal/githubapp"
	"github.com/hoshinojian/ciorch/internal/incident"
	"github.com/hoshinojian/ciorch/internal/server"
)

// 审查修复 1：secret 文件末尾换行被去掉；过短 secret 拒绝。
func TestReadWebhookSecretTrimsAndValidates(t *testing.T) {
	dir := t.TempDir()
	// shell 重定向生成的文件普遍带末尾换行
	p := filepath.Join(dir, "webhook.secret")
	if err := os.WriteFile(p, []byte("a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := readWebhookSecret(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6" {
		t.Fatalf("应去末尾换行：%q", secret)
	}
	// 空白/过短拒绝
	for _, bad := range []string{"\n", "short\n", "   \n"} {
		bp := filepath.Join(dir, "bad.secret")
		os.WriteFile(bp, []byte(bad), 0o600)
		if _, err := readWebhookSecret(bp); err == nil {
			t.Fatalf("%q 应拒绝（<16 字节 = 任何人可伪造）", bad)
		}
	}
}

// 审查修复 1+2 端到端：secret 文件带换行，用不带换行的 secret 签名的
// issues.closed webhook → 验签通过 → 事故模块 HumanResolved → 熔断被清。
func TestWebhookIssuesClosedClearsBreakerEndToEnd(t *testing.T) {
	// secret：文件带换行（runbook 的 openssl 生成方式），GitHub 签名不带
	secretFile := filepath.Join(t.TempDir(), "webhook.secret")
	const secret = "0123456789abcdef0123456789abcdef"
	if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readWebhookSecret(secretFile)
	if err != nil {
		t.Fatal(err)
	}

	// 引擎：预置 open 熔断
	cfg := &config.Config{
		StateDir: t.TempDir(),
		Global:   config.GlobalCfg{MaxMachines: 2, DailyBudgetUSD: 5},
		Profiles: map[string]config.Profile{"go-heavy": {Pools: []string{"p"}, RunnersPerDroplet: 1}},
		Repos:    []config.RepoEntry{{Repo: "o/runner", Profile: "go-heavy", MaxRunners: 2}},
	}
	eng := engine.New(cfg, nil, nil, nil, nil, nil, log.New(io.Discard, "", 0))
	eng.States["o/runner"] = decide.RepoState{Breaker: decide.Breaker{State: "open", Fails: 3}}

	// 事故模块：issue #1 已开（复用 fake），托管仓 o/runner
	issues := &e2eFakeIssues{next: 1}
	mgr := incident.New(issues, eng, incident.Options{MaintainerLogin: "maint"}, t.TempDir(), log.New(io.Discard, "", 0))
	mgr.Now = func() time.Time { return time.Now() }
	if err := mgr.Handle(context.Background(), "o/runner", decide.Action{Kind: decide.ActIncident, IncidentType: "B", IncidentKey: "m1", Reason: "熔断"}); err != nil {
		t.Fatal(err)
	}
	if issues.created != 1 {
		t.Fatalf("前置：应开 1 个 issue，got %d", issues.created)
	}

	// server：模拟 cmd 装配（OnIssueClosed 桥接 + 白名单）
	h := server.New(server.Deps{
		WebhookSecret: got,
		Repos:         map[string]bool{"o/runner": true},
		OnIssueClosed: func(repo string, number int) { mgr.HumanResolved(repo, int64(number)) },
	})
	body := []byte(fmt.Sprintf(`{"action":"closed","repository":{"full_name":"o/runner"},
		"issue":{"number":1,"labels":[{"name":"ciorch"}]},
		"sender":{"login":"maintainer","type":"User"}}`))
	mac := hmac.New(sha256.New, []byte(secret)) // 签名用不带换行的 secret
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("带换行 secret 文件应能验签通过：%d %s", rec.Code, rec.Body.String())
	}
	if st := eng.States["o/runner"]; st.Breaker.State != "closed" || st.Breaker.Fails != 0 {
		t.Fatalf("人工关闭 issue 应清熔断：%+v", st.Breaker)
	}
}

// e2eFakeIssues：cmd 级端到端用的最小 issue 假实现。
type e2eFakeIssues struct {
	next, created int
}

func (f *e2eFakeIssues) EnsureLabels(ctx context.Context, repo string, labels []string) error {
	return nil
}
func (f *e2eFakeIssues) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (githubapp.Issue, error) {
	f.created++
	return githubapp.Issue{Number: int64(f.created), State: "open"}, nil
}
func (f *e2eFakeIssues) AddIssueComment(ctx context.Context, repo string, number int64, body string) error {
	return nil
}
func (f *e2eFakeIssues) AddIssueLabels(ctx context.Context, repo string, number int64, labels []string) error {
	return nil
}
func (f *e2eFakeIssues) CloseIssue(ctx context.Context, repo string, number int64) error { return nil }
func (f *e2eFakeIssues) ReopenIssue(ctx context.Context, repo string, number int64) error {
	return nil
}

// plan / daemon -dry-run 必须零副作用：事故模块也要 DryRun，否则 D/E/G 等
// 事故场景会真开 issue、真调 LLM、真执行 LLM 动作（engine dry-run 分支仍
// fireIncident）。只读装配之外（真 daemon）不得误开 DryRun。
func TestWiringReadOnlyMarksIncidentsDryRun(t *testing.T) {
	cfg := &config.Config{
		StateDir: t.TempDir(),
		Pools: map[string]config.PoolCfg{"p": {Provider: "vpsctl", MaxMachines: 1,
			Raw: []byte(`{"bin":"/bin/true","account":"a","region":"sgp1","size":"s","image":"i","hourly_cost_hint":0.1}`)}},
		Global:   config.GlobalCfg{MaxMachines: 1, DailyBudgetUSD: 1},
		Profiles: map[string]config.Profile{"g": {Pools: []string{"p"}, RunnersPerDroplet: 1}},
		Repos:    []config.RepoEntry{{Repo: "o/r", Profile: "g", MaxRunners: 1}},
	}
	for _, readOnly := range []bool{true, false} {
		rt, err := wiring(cfg, "TESTTOKEN123456", readOnly)
		if err != nil {
			t.Fatal(err)
		}
		if rt.incidents.DryRun != readOnly {
			t.Fatalf("readOnly=%v：incidents.DryRun 应为 %v", readOnly, readOnly)
		}
	}
}
