package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoshinojian/ciorch/internal/machine"
	"github.com/hoshinojian/ciorch/internal/machine/machinetest"
)

// 测试用假实现：config 不依赖任何真实提供商，校验路径经 Registry 走 fake
// （真实实现的私有配置校验见 internal/provider/vpsctl 的 Factory 测试）。
func init() {
	machine.Register("fakeprov", func(pool string, raw json.RawMessage, env machine.Env) (machine.Provider, error) {
		var o struct {
			Account string `json:"account"`
		}
		if len(raw) == 0 {
			return nil, errString("pools." + pool + ".fakeprov：缺配置子对象")
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
		if o.Account == "" {
			return nil, errString("pools." + pool + ".fakeprov.account：必填")
		}
		return machinetest.NewFake(pool), nil
	})
}

type errString string

func (e errString) Error() string { return string(e) }

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ciorch.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 池化 schema（§3.3）的最小合法配置。
const validCfg = `{
  "pools": {
    "do-sgp1-c2": {
      "provider": "fakeprov",
      "max_machines": 2,
      "fakeprov": {"account": "team2-ci"}
    }
  },
  "state_dir": "/var/lib/ciorch",
  "global": {"max_machines": 2, "daily_budget_usd": 5.0},
  "profiles": {
    "go-heavy": {"pools": ["do-sgp1-c2"], "labels": ["ci"], "runners_per_droplet": 1}
  },
  "repos": [
    {"repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 2, "issue_repo": ""}
  ]
}`

func TestLoadValidFullSchema(t *testing.T) {
	c, err := Load(write(t, validCfg))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pools) != 1 || c.Pools["do-sgp1-c2"].Provider != "fakeprov" || c.Pools["do-sgp1-c2"].MaxMachines != 2 {
		t.Fatalf("pools 解析不对：%+v", c.Pools)
	}
	if got := c.PoolNameOf(c.Profiles["go-heavy"]); got != "do-sgp1-c2" {
		t.Fatalf("PoolNameOf：%q", got)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name, want string
		body       string
	}{
		{"未知 provider", "未知 provider", strings.ReplaceAll(validCfg, "fakeprov", "no-such")},
		{"实现子对象与 provider 不同名", "不匹配", strings.Replace(validCfg, `"fakeprov": {"account": "team2-ci"}`, `"otherprov": {"account": "x"}`, 1)},
		{"私有配置缺失", "缺配置子对象", strings.Replace(validCfg, ",\n      \"fakeprov\": {\"account\": \"team2-ci\"}", "", 1)},
		{"私有字段缺必填", "fakeprov.account：必填", strings.Replace(validCfg, `{"account": "team2-ci"}`, `{}`, 1)},
		{"池容量 0", "max_machines：必须 ≥1", strings.Replace(validCfg, `"max_machines": 2,`, `"max_machines": 0,`, 1)},
		{"profile 引用不存在的池", "不存在于 pools", strings.Replace(validCfg, `"pools": ["do-sgp1-c2"]`, `"pools": ["no-such"]`, 1)},
		{"profile 无池", "至少挂 1 个池", strings.Replace(validCfg, `"pools": ["do-sgp1-c2"], "labels"`, `"pools": [], "labels"`, 1)},
		{"global 上限小于 max_runners", "必须 ≥ 任一仓的 max_runners", strings.Replace(validCfg, `"max_machines": 2, "daily_budget_usd"`, `"max_machines": 1, "daily_budget_usd"`, 1)},
		{"预算为 0", "daily_budget_usd：必须 >0", strings.Replace(validCfg, `"daily_budget_usd": 5.0`, `"daily_budget_usd": 0`, 1)},
		{"state_dir 缺失", "state_dir：必填", strings.Replace(validCfg, `"state_dir": "/var/lib/ciorch",`, ``, 1)},
		{"label 非法字符", "不满足", strings.Replace(validCfg, `"labels": ["ci"]`, `"labels": ["cisandbox!"]`, 1)},
		{"repo 重复", "重复", strings.Replace(validCfg, `"issue_repo": ""`, `"issue_repo": ""}, {"repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 1`, 1)},
	}
	for _, c := range cases {
		_, err := Load(write(t, c.body))
		if err == nil {
			t.Errorf("%s：应报错", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息不含 %q：%v", c.name, c.want, err)
		}
	}
}

// §6 config：profile 挂 2 个池时报"多池调度尚未实现"。
func TestProfileTwoPoolsRejected(t *testing.T) {
	body := strings.Replace(validCfg, `"pools": ["do-sgp1-c2"]`, `"pools": ["do-sgp1-c2", "do-blr1-c2"]`, 1)
	body = strings.Replace(body, `"fakeprov": {"account": "team2-ci"}\n    }`,
		`"fakeprov": {"account": "team2-ci"}\n    },\n    "do-blr1-c2": {"provider": "fakeprov", "max_machines": 1, "fakeprov": {"account": "team2-ci"}}`, 1)
	_, err := Load(write(t, body))
	if err == nil || !strings.Contains(err.Error(), "多池调度尚未实现") {
		t.Fatalf("两个池应报\"多池调度尚未实现\"：err=%v", err)
	}
}

// 未知顶层字段仍拒绝（DisallowUnknownFields 不变量保持）。
func TestLoadRejectsUnknownFields(t *testing.T) {
	body := strings.Replace(validCfg, `"state_dir"`, `"stated_dir_typo"`, 1)
	if _, err := Load(write(t, body)); err == nil {
		t.Fatal("拼错的键应整体被拒（而非静默走默认值）")
	}
}

// §3.3：旧字段给出明确迁移提示（指向实施计划），不做兼容解析。
func TestLegacyFieldsMigrationHint(t *testing.T) {
	legacy := `{
  "vpsctl_bin": "/usr/local/bin/vpsctl",
  "vpsctl_accounts_file": "/etc/ciorch/vpsctl-accounts.json",
  "state_dir": "/var/lib/ciorch",
  "global": {"max_droplets": 2, "daily_budget_usd": 5.0},
  "profiles": {
    "go-heavy": {"account": "team2-ci", "region": "sgp1", "size": "c2-4vcpu-8gb", "image": "ubuntu-24-04-x64", "labels": ["ci"], "runners_per_droplet": 1, "fallback_regions": ["sgp1"]}
  },
  "repos": [{"repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 2}]
}`
	_, err := Load(write(t, legacy))
	if err == nil {
		t.Fatal("旧 schema 应被拒绝（不做兼容解析）")
	}
	for _, want := range []string{
		"旧 schema", "vpsctl_bin", "global.max_droplets", "profiles.go-heavy.account",
		"PROVIDER-ABSTRACTION.md §3.3", "hourly_cost_hint",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("迁移提示应含 %q：%v", want, err)
		}
	}
}

// 示例配置 schema 不漂移（占位符/真实 provider 换成假实现后应可加载）。
func TestExampleConfigSchema(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "configs", "ciorch.example.json"))
	if err != nil {
		t.Fatalf("示例配置应入库 configs/ciorch.example.json：%v", err)
	}
	body := string(raw)
	body = strings.Replace(body, `"provider": "vpsctl"`, `"provider": "fakeprov"`, 1)
	start := strings.Index(body, `"vpsctl": {`)
	end := start + strings.Index(body[start:], "\n      }")
	body = body[:start] + `"fakeprov": {"account": "team2-ci"}` + body[end+len("\n      }"):]
	body = strings.ReplaceAll(body, `"<owner>/<name>"`, `"hoshinojian/nms"`)
	body = strings.ReplaceAll(body, `"<maintainer-login>"`, `"someone"`)
	if _, err := Load(write(t, body)); err != nil {
		t.Fatalf("示例配置（provider 换假实现后）应可加载：%v", err)
	}
}

func TestNamingHelpers(t *testing.T) {
	a := RepoEntry{Repo: "hoshinojian/nms"}
	b := RepoEntry{Repo: "hoshinojian/other-nms"}
	if a.Tag() == b.Tag() {
		t.Fatalf("不同仓 tag 不得相同: %s", a.Tag())
	}
	if !strings.HasPrefix(a.Tag(), "ciorch-r-hoshinojian-nms-") {
		t.Fatalf("tag 前缀不对: %s", a.Tag())
	}
	if len(a.Tag()) > 64 {
		t.Fatalf("tag 总长 ≤64: %s (%d)", a.Tag(), len(a.Tag()))
	}
	if a.NamePrefix() != "ci-nms" {
		t.Fatalf("名字前缀不对: %s", a.NamePrefix())
	}
	// 大写/特殊字符 sanitize
	c := RepoEntry{Repo: "A/B.C_D"}
	if got := c.NamePrefix(); got != "ci-b-c-d" {
		t.Fatalf("sanitize 不对: %s", got)
	}
}

func TestResolveTokenPriority(t *testing.T) {
	gh := func() (string, error) { return "GHFALLBACK_TOKEN_12345", nil }
	t.Setenv("CIORCH_TOKEN", "")
	if tok, _ := ResolveToken(nil, "EXPLICIT_TOKEN_12345", gh); tok != "EXPLICIT_TOKEN_12345" {
		t.Fatalf("显式参数应最优先，got %q", tok)
	}
	t.Setenv("CIORCH_TOKEN", "ENV_TOKEN_1234567890")
	if tok, _ := ResolveToken(nil, "", gh); tok != "ENV_TOKEN_1234567890" {
		t.Fatalf("环境变量应次之，got %q", tok)
	}
	t.Setenv("CIORCH_TOKEN", "")
	path := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(path, []byte("  FILE_TOKEN_1234567 \n"), 0o600)
	c := &Config{TokenFile: path}
	if tok, _ := ResolveToken(c, "", gh); tok != "FILE_TOKEN_1234567" {
		t.Fatalf("token_file 应去空白，got %q", tok)
	}
	if tok, _ := ResolveToken(nil, "", gh); tok != "GHFALLBACK_TOKEN_12345" {
		t.Fatalf("gh auth token 应兜底，got %q", tok)
	}
}

func TestResolveTokenNothingAvailable(t *testing.T) {
	t.Setenv("CIORCH_TOKEN", "")
	if _, err := ResolveToken(nil, "", nil); err == nil {
		t.Fatal("无任何 token 源应报错")
	}
}
