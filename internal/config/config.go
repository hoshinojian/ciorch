// Package config 加载并校验 ciorch 配置。
// 阶段 1 只承载 stats 所需子集（repos + token 源）；完整 schema 见
// docs/REDESIGN.md §3.3，随阶段 2 决策核心落地。
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// Config 是阶段 1 配置子集。
type Config struct {
	// Repos 是托管的仓列表（owner/name）。
	Repos []string `json:"repos"`
	// TokenFile 是 GitHub token 文件路径；空 = 走环境变量/gh auth token 兜底。
	TokenFile string `json:"token_file"`
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Load 读取并校验配置（错误信息中文、指出字段路径）。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if len(c.Repos) == 0 {
		return nil, fmt.Errorf("repos：至少配置一个仓（owner/name）")
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		if !repoRe.MatchString(r) {
			return nil, fmt.Errorf("repos[%d]：%q 不是 owner/name 形式", i, r)
		}
		if seen[r] {
			return nil, fmt.Errorf("repos[%d]：%q 重复", i, r)
		}
		seen[r] = true
	}
	return &c, nil
}

// ResolveToken 按优先级解析 GitHub token：显式参数 > 环境变量 CIORCH_TOKEN >
// token_file > gh auth token 子进程兜底（阶段 1 临时 token 源，App 认证阶段 3 替换）。
func ResolveToken(c *Config, explicit string, runGH func() (string, error)) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("CIORCH_TOKEN"); env != "" {
		return env, nil
	}
	if c != nil && c.TokenFile != "" {
		raw, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return "", fmt.Errorf("读取 token_file %s: %w", c.TokenFile, err)
		}
		tok := string(bytes.TrimSpace(raw))
		if len(tok) < 16 {
			return "", fmt.Errorf("token_file %s 内容异常（长度 %d）", c.TokenFile, len(tok))
		}
		return tok, nil
	}
	if runGH != nil {
		tok, err := runGH()
		if err != nil {
			return "", fmt.Errorf("gh auth token 兜底失败: %w", err)
		}
		return tok, nil
	}
	return "", fmt.Errorf("无 token 可用：需要 -token、CIORCH_TOKEN、token_file 或 gh 登录态之一")
}
