// Package config 加载并校验 ciorch 配置（schema 对齐 docs/REDESIGN.md §3.3）。
// 未知字段一律拒绝（DisallowUnknownFields）：调度参数拼错键名而静默走默认值
// （如 max_runners→1）的风险，大于前向兼容损失（配置与二进制同版本演进）。
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// GitHubAppCfg 是 GitHub App 凭据位置（认证实现在阶段 3，schema 先行）。
type GitHubAppCfg struct {
	AppID             int64  `json:"app_id"`
	PrivateKeyFile    string `json:"private_key_file"`
	WebhookSecretFile string `json:"webhook_secret_file"`
}

// MachineAgentCfg 是机器侧 cirunner 二进制的版本钉扎（cloud-init 下载校验用）。
type MachineAgentCfg struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// GlobalCfg 是全局配额与预算。
type GlobalCfg struct {
	MaxDroplets    int               `json:"max_droplets"`
	DailyBudgetUSD float64           `json:"daily_budget_usd"`
	Tuning         map[string]string `json:"tuning"` // 决策参数覆盖（键名见 §3.6.6，decide 侧解析）
}

// LLMCfg 是 DeepSeek 诊断配置（事故管线阶段 5 用，schema 先行）。
type LLMCfg struct {
	BaseURL             string `json:"base_url"`
	Model               string `json:"model"`
	APIKeyFile          string `json:"api_key_file"`
	MaxCallsPerIncident int    `json:"max_calls_per_incident"`
	MaxCallsPerDay      int    `json:"max_calls_per_day"`
}

// Profile 是一类 runner 机器的建机配方。
type Profile struct {
	Account           string   `json:"account"`
	Region            string   `json:"region"`
	FallbackRegions   []string `json:"fallback_regions"`
	Size              string   `json:"size"`
	Image             string   `json:"image"`
	Snapshot          string   `json:"snapshot"`
	Labels            []string `json:"labels"`
	RunnersPerDroplet int      `json:"runners_per_droplet"`
}

// RepoEntry 是一个托管仓。
type RepoEntry struct {
	Repo       string `json:"repo"`
	Profile    string `json:"profile"`
	MaxRunners int    `json:"max_runners"`
	IssueRepo  string `json:"issue_repo"`
}

// Config 是完整配置。
type Config struct {
	GitHubApp          GitHubAppCfg       `json:"github_app"`
	VPSctlBin          string             `json:"vpsctl_bin"`
	VPSctlAccountsFile string             `json:"vpsctl_accounts_file"`
	SSHPrivateKeyFile  string             `json:"ssh_private_key_file"`
	SSHPublicKeyFiles  []string           `json:"ssh_public_key_files"`
	MachineAgent       MachineAgentCfg    `json:"machine_agent"`
	Listen             string             `json:"listen"`
	StateDir           string             `json:"state_dir"`
	Global             GlobalCfg          `json:"global"`
	LLM                LLMCfg             `json:"llm"`
	MaintainerLogin    string             `json:"maintainer_login"`
	Profiles           map[string]Profile `json:"profiles"`
	Repos              []RepoEntry        `json:"repos"`
	TokenFile          string             `json:"token_file"` // 阶段 1-2 临时 token 源（App 认证前）
}

var (
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	labelRe  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	regionRe = regexp.MustCompile(`^[a-z]+[0-9]+$`) // DO slug 形如 sgp1/nyc3/blr1（无连字符）
)

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
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("配置 %s 无效: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	if len(c.Repos) == 0 {
		return fmt.Errorf("repos：至少配置一个仓（owner/name）")
	}
	if len(c.Profiles) == 0 {
		return fmt.Errorf("profiles：至少配置一个 profile")
	}
	for name, p := range c.Profiles {
		if p.Account == "" {
			return fmt.Errorf("profiles.%s.account：必填（vpsctl -only 钉账号，防全账号误建）", name)
		}
		if p.Region == "" {
			return fmt.Errorf("profiles.%s.region：必填", name)
		}
		if !regionRe.MatchString(p.Region) {
			return fmt.Errorf("profiles.%s.region：%q 不是 DO region slug 形式", name, p.Region)
		}
		if p.Size == "" {
			return fmt.Errorf("profiles.%s.size：必填", name)
		}
		if p.Image == "" {
			return fmt.Errorf("profiles.%s.image：必填", name)
		}
		for i, l := range p.Labels {
			if !labelRe.MatchString(l) {
				return fmt.Errorf("profiles.%s.labels[%d]：%q 不满足 ^[A-Za-z0-9_-]+$（self-hosted/linux/x64 为注册自动标签，不必写）", name, i, l)
			}
		}
		if p.RunnersPerDroplet == 0 {
			p.RunnersPerDroplet = 1
			c.Profiles[name] = p
		}
		if p.RunnersPerDroplet < 1 || p.RunnersPerDroplet > 2 {
			return fmt.Errorf("profiles.%s.runners_per_droplet：取值 1~2（2 为实验特性）", name)
		}
		for i, r := range p.FallbackRegions {
			if !regionRe.MatchString(r) {
				return fmt.Errorf("profiles.%s.fallback_regions[%d]：%q 不是 DO region slug 形式", name, i, r)
			}
		}
	}
	seen := map[string]bool{}
	maxRunners := 0
	for i, r := range c.Repos {
		if !repoRe.MatchString(r.Repo) {
			return fmt.Errorf("repos[%d].repo：%q 不是 owner/name 形式", i, r.Repo)
		}
		if seen[r.Repo] {
			return fmt.Errorf("repos[%d].repo：%q 重复", i, r.Repo)
		}
		seen[r.Repo] = true
		if _, ok := c.Profiles[r.Profile]; !ok {
			return fmt.Errorf("repos[%d].profile：%q 不存在于 profiles", i, r.Profile)
		}
		if r.MaxRunners < 1 {
			return fmt.Errorf("repos[%d].max_runners：必须 ≥1", i)
		}
		if r.IssueRepo != "" && !repoRe.MatchString(r.IssueRepo) {
			return fmt.Errorf("repos[%d].issue_repo：%q 不是 owner/name 形式（空 = 在本仓开 issue）", i, r.IssueRepo)
		}
		if r.MaxRunners > maxRunners {
			maxRunners = r.MaxRunners
		}
	}
	if c.Global.MaxDroplets < maxRunners {
		return fmt.Errorf("global.max_droplets（%d）：必须 ≥ 任一仓的 max_runners（最大 %d）", c.Global.MaxDroplets, maxRunners)
	}
	if c.Global.DailyBudgetUSD <= 0 {
		return fmt.Errorf("global.daily_budget_usd：必须 >0（钱有硬上限，§2.2 原则 10）")
	}
	return nil
}

// ProfileOf 返回仓对应的 profile（Load 已校验存在；找不到返回零值）。
func (c *Config) ProfileOf(r RepoEntry) Profile {
	return c.Profiles[r.Profile]
}

// sanitize 归一化为 tag/名字安全片段：小写、非 [a-z0-9] 替换 -、截断到 max。
func sanitize(s string, max int) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// sha256Prefix 返回 s 的 sha256 hex 前 n 位。
func sha256Prefix(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:n]
}

// Tag 返回仓的 droplet tag（§3.5：ciorch-r-<owner>-<name>-<sha256 前 8 位>，
// sanitize 后总长 ≤64——用 owner/name 各截一段保证）。
func (r RepoEntry) Tag() string {
	parts := strings.SplitN(r.Repo, "/", 2)
	owner, name := sanitize(parts[0], 20), sanitize(parts[1], 20)
	sum := sha256Prefix(r.Repo, 8)
	return fmt.Sprintf("ciorch-r-%s-%s-%s", owner, name, sum)
}

// NamePrefix 返回 droplet 名前缀（vpsctl -name-prefix）：ci-<sanitize(name) 截 20>。
func (r RepoEntry) NamePrefix() string {
	parts := strings.SplitN(r.Repo, "/", 2)
	return "ci-" + sanitize(parts[1], 20)
}

// NamespaceRe 返回本仓命名空间正则（droplet/runner 名 = {prefix}-{account}-
// {region}-NN；命名空间外绝不触碰，§3.5 归属判定）。
func (r RepoEntry) NamespacePattern(p Profile) string {
	return fmt.Sprintf(`^%s-%s-%s-\d{2}$`, regexp.QuoteMeta(r.NamePrefix()), regexp.QuoteMeta(p.Account), regexp.QuoteMeta(p.Region))
}

// ResolveToken 按优先级解析 GitHub token：显式参数 > 环境变量 CIORCH_TOKEN >
// token_file > gh auth token 子进程兜底（阶段 1-2 临时 token 源，App 认证阶段 3 替换）。
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
