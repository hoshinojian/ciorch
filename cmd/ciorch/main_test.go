package main

import (
	"testing"
	"time"
)

// -config 两种形式、子命令前后均可（§3.13 口径）。
func TestParseArgsConfigForms(t *testing.T) {
	cases := []struct {
		args []string
		want parsedArgs
	}{
		{[]string{"-config", "/x/c.json", "stats", "-since", "7d"}, parsedArgs{"/x/c.json", "stats", []string{"-since", "7d"}}},
		{[]string{"-config=/x/c.json", "stats"}, parsedArgs{"/x/c.json", "stats", nil}},
		{[]string{"stats", "-config", "/x/c.json", "-repo", "a/b"}, parsedArgs{"/x/c.json", "stats", []string{"-repo", "a/b"}}},
		{[]string{"stats"}, parsedArgs{"", "stats", nil}},
	}
	for _, c := range cases {
		got, err := parseArgs(c.args)
		if err != nil {
			t.Fatalf("%v 应解析成功: %v", c.args, err)
		}
		if got.configPath != c.want.configPath || got.sub != c.want.sub || len(got.subArgs) != len(c.want.subArgs) {
			t.Fatalf("%v 解析不对: %+v（want %+v）", c.args, got, c.want)
		}
	}
}

// 审查修正 3 的回归：只有全局 flag、没有子命令 → 返回错误，不得 panic。
func TestParseArgsNoSubcommandErrors(t *testing.T) {
	cases := [][]string{
		{},
		{"-config", "/x/c.json"},
		{"-config=/x/c.json"},
		{"-token", "abc"},
	}
	for _, args := range cases {
		if _, err := parseArgs(args); err == nil {
			t.Errorf("%v 应报缺少子命令", args)
		}
	}
}

func TestParseWindow(t *testing.T) {
	if d, err := parseWindow("7d"); err != nil || d != 7*24*time.Hour {
		t.Fatalf("7d 应为 168h: %v %v", d, err)
	}
	if d, err := parseWindow("90d"); err != nil || d != 90*24*time.Hour {
		t.Fatalf("90d 应为 2160h: %v %v", d, err)
	}
	if d, err := parseWindow("24h"); err != nil || d != 24*time.Hour {
		t.Fatalf("24h 应为 24h: %v %v", d, err)
	}
	for _, bad := range []string{"0d", "100d", "7x", "abc", "-1h"} {
		if _, err := parseWindow(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}
