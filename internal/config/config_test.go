package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ciorch.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// §3.3 示例的最小可用版本（含全部必填字段）。
const validCfg = `{
  "github_app": {"app_id": 123456, "private_key_file": "/etc/ciorch/app.pem", "webhook_secret_file": "/etc/ciorch/webhook.secret"},
  "token_file": "/tmp/tok",
  "listen": "127.0.0.1:8080",
  "state_dir": "/var/lib/ciorch",
  "global": {"max_droplets": 2, "daily_budget_usd": 5.0},
  "maintainer_login": "hoshinojian",
  "profiles": {
    "go-heavy": {"account": "team2-ci", "region": "sgp1", "size": "c2-4vcpu-8gb", "image": "ubuntu-24-04-x64", "labels": ["ci"]}
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
	r := c.Repos[0]
	if r.Repo != "hoshinojian/nms" || r.MaxRunners != 2 {
		t.Fatalf("repo 解析不对: %+v", r)
	}
	p := c.ProfileOf(r)
	if p.Account != "team2-ci" || p.Region != "sgp1" || p.RunnersPerDroplet != 1 {
		t.Fatalf("profile 解析不对（runners_per_droplet 应默认 1）: %+v", p)
	}
}

func TestLoadValidationErrors(t *testing.T) {
	replace := func(old, new string) string {
		return strings.Replace(validCfg, old, new, 1)
	}
	cases := map[string]struct {
		body string
		want string
	}{
		"profile 缺 account":      {replace(`"account": "team2-ci"`, `"account": ""`), "account：必填"},
		"仓引用不存在的 profile":        {replace(`"profile": "go-heavy"`, `"profile": "nope"`), "不存在于 profiles"},
		"max_runners < 1":        {replace(`"max_runners": 2`, `"max_runners": 0`), "max_runners：必须 ≥1"},
		"全局配额小于仓上限":              {replace(`"max_droplets": 2`, `"max_droplets": 1`), "max_droplets"},
		"预算非正":                   {replace(`"daily_budget_usd": 5.0`, `"daily_budget_usd": 0`), "daily_budget_usd"},
		"repo 形式错":               {replace(`"hoshinojian/nms"`, `"nms"`), "owner/name 形式"},
		"repo 重复":                {replace(`{"repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 2, "issue_repo": ""}`, `{"repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 1, "issue_repo": ""},{"repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 1, "issue_repo": ""}`), "重复"},
		"label 非法字符":             {replace(`"labels": ["ci"]`, `"labels": ["ci:bad"]`), "^[A-Za-z0-9_-]+$"},
		"region 形式错":             {replace(`"region": "sgp1"`, `"region": "SGP-1"`), "region slug"},
		"runners_per_droplet 越界": {replace(`"labels": ["ci"]`, `"labels": ["ci"], "runners_per_droplet": 3`), "1~2"},
		"issue_repo 形式错":         {replace(`"issue_repo": ""`, `"issue_repo": "x"`), "issue_repo"},
		"repos 字符串数组":            {`{"repos":["a/b"]}`, "解析配置"},
	}
	for name, c := range cases {
		if _, err := Load(write(t, c.body)); err == nil {
			t.Errorf("%s：应报错", name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s：错误信息应含 %q，got %q", name, c.want, err.Error())
		}
	}
}

// 未知字段拒绝（DisallowUnknownFields）：拼写错误在加载期暴露。
func TestLoadRejectsUnknownFields(t *testing.T) {
	cases := []string{
		strings.Replace(validCfg, `"max_droplets": 2`, `"max_droplets": 2, "max_droples": 3`, 1),
		strings.Replace(validCfg, `{"repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 2, "issue_repo": ""}`, `{"repo": "hoshinojian/nms", "profile": "go-heavy", "max_runners": 2, "issue_repo": "", "profil": "x"}`, 1),
	}
	for _, body := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("含未知字段应报错：%s", body)
		}
	}
}

// §3.5 归属命名：tag 带哈希防短名撞车；前缀取仓名 sanitize；总长 ≤64。
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

// 命名空间正则：本仓名匹配、外部 runner/不同区域/前缀嫁接不匹配。
func TestNamespacePattern(t *testing.T) {
	r := RepoEntry{Repo: "hoshinojian/nms"}
	p := Profile{Account: "team2-ci", Region: "sgp1"}
	re := regexp.MustCompile(r.NamespacePattern(p))
	cases := map[string]bool{
		"ci-nms-team2-ci-sgp1-01":  true,
		"ci-nms-team2-ci-sgp1-99":  true,
		"gitrunner-sgp2":           false,
		"ci-nms-team2-ci-sgp2-01":  false, // 不同区域
		"ci-nms-team2-ci-sgp1-xx":  false,
		"xci-nms-team2-ci-sgp1-01": false, // 前缀嫁接
	}
	for name, want := range cases {
		if got := re.MatchString(name); got != want {
			t.Errorf("NamespacePattern(%q)=%v want %v", name, got, want)
		}
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
