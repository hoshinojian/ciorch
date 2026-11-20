// Package config 加载并校验 ciorch 配置。
// schema 对齐 docs/REDESIGN.md §3.3：repos 为对象数组。阶段 1 只解析并校验
// repo 字段（stats 只需要它）；profile/max_runners/issue_repo 等字段随阶段 2
// 决策核心实现时加入——未知字段一律拒绝（DisallowUnknownFields），理由：
// 调度参数拼错键名而静默走默认值（如 max_runners→1）的风险，大于失去的
// 前向兼容（本仓配置与二进制同版本演进，不存在旧二进制读新配置的场景）。
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// RepoEntry 是一个托管仓的配置（阶段 1 仅 repo 字段，其余阶段 2 落地）。
type RepoEntry struct {
	Repo string `json:"repo"`
}

// Config 是阶段 1 配置子集。
type Config struct {
	// Repos 是托管的仓列表（对象数组，§3.3）。
	Repos []RepoEntry `json:"repos"`
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("解析配置 %s: %w", path, err)
	}
	if len(c.Repos) == 0 {
		return nil, fmt.Errorf("repos：至少配置一个仓（owner/name）")
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		if !repoRe.MatchString(r.Repo) {
			return nil, fmt.Errorf("repos[%d].repo：%q 不是 owner/name 形式", i, r.Repo)
		}
		if seen[r.Repo] {
			return nil, fmt.Errorf("repos[%d].repo：%q 重复", i, r.Repo)
		}
		seen[r.Repo] = true
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
