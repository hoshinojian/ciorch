package config

import (
	"os"
	"path/filepath"
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

func TestLoadValid(t *testing.T) {
	c, err := Load(write(t, `{"repos":["hoshinojian/nms","a/b.c-d"],"token_file":"/x/tok"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Repos) != 2 || c.Repos[0] != "hoshinojian/nms" || c.TokenFile != "/x/tok" {
		t.Fatalf("解析不对: %+v", c)
	}
}

func TestLoadErrorsChineseWithFieldPath(t *testing.T) {
	cases := map[string]string{
		`{"repos":[]}`:            "至少配置一个仓",
		`{"repos":["norepo"]}`:    "owner/name 形式",
		`{"repos":["a/b","a/b"]}`: "重复",
		`not json`:                "解析配置",
	}
	for body, want := range cases {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%q 应报错", body)
		} else if !strings.Contains(err.Error(), want) {
			t.Errorf("%q 错误信息应含 %q，got %q", body, want, err.Error())
		}
	}
}

func TestResolveTokenPriority(t *testing.T) {
	gh := func() (string, error) { return "GHFALLBACK_TOKEN_12345", nil }
	t.Setenv("CIORCH_TOKEN", "")
	// 显式参数最优先
	if tok, _ := ResolveToken(nil, "EXPLICIT_TOKEN_12345", gh); tok != "EXPLICIT_TOKEN_12345" {
		t.Fatalf("显式参数应最优先，got %q", tok)
	}
	// 环境变量次之
	t.Setenv("CIORCH_TOKEN", "ENV_TOKEN_1234567890")
	if tok, _ := ResolveToken(nil, "", gh); tok != "ENV_TOKEN_1234567890" {
		t.Fatalf("环境变量应次之，got %q", tok)
	}
	// token_file 再次
	t.Setenv("CIORCH_TOKEN", "")
	path := filepath.Join(t.TempDir(), "tok")
	os.WriteFile(path, []byte("  FILE_TOKEN_1234567 \n"), 0o600)
	c := &Config{TokenFile: path}
	if tok, _ := ResolveToken(c, "", gh); tok != "FILE_TOKEN_1234567" {
		t.Fatalf("token_file 应去空白，got %q", tok)
	}
	// gh 兜底最后
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
