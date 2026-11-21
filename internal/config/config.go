// Package config 加载并校验 ciorch 配置（schema 对齐 docs/REDESIGN.md §3.3
// 与 docs/PROVIDER-ABSTRACTION.md §3.3：机器来源池化，profile 只描述 runner）。
// 未知字段一律拒绝（DisallowUnknownFields）：调度参数拼错键名而静默走默认值
// （如 max_runners→1）的风险，大于前向兼容损失（配置与二进制同版本演进）。
// 遇到旧字段（vpsctl_bin/max_droplets/profile 内建机参数等）给出明确迁移
// 提示，不做向后兼容解析——现存配置只有 shadow 用的一份，一次到位。
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

	"github.com/hoshinojian/ciorch/internal/machine"
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
	MaxMachines    int               `json:"max_machines"`
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

// PoolCfg 是一个机器池：一组同来源同规格的机器。provider 选实现；与
// provider 同名的子对象是该实现的私有配置（只有对应实现解析，其余字段
// 一律拒绝）；max_machines 是池容量上限。
type PoolCfg struct {
	Provider    string
	MaxMachines int
	Raw         json.RawMessage // 实现私有配置子对象（Factory 解析）
}

// UnmarshalJSON 严格解析：只允许 provider / max_machines / <与 provider 同名
// 的实现子对象> 三个键（外层 DisallowUnknownFields 对自定义 Unmarshaler 不
// 生效，严格性由本方法保证）。
func (p *PoolCfg) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	get := func(key string, dst any) error {
		raw, ok := m[key]
		if !ok {
			return nil
		}
		delete(m, key)
		return json.Unmarshal(raw, dst)
	}
	if err := get("provider", &p.Provider); err != nil {
		return err
	}
	if err := get("max_machines", &p.MaxMachines); err != nil {
		return err
	}
	var extra []string
	for k := range m {
		extra = append(extra, k)
	}
	switch {
	case len(extra) == 0:
		// 无实现子对象：由 Factory 报"缺配置"
	case len(extra) > 1:
		return fmt.Errorf("池配置只允许 provider/max_machines/<实现子对象>，多余字段：%v", extra)
	default:
		if extra[0] != p.Provider {
			return fmt.Errorf("池配置子对象 %q 与 provider %q 不匹配（实现私有配置须以 provider 名为键）", extra[0], p.Provider)
		}
		p.Raw = m[extra[0]]
	}
	return nil
}

// Profile 是一类 runner 的配方：长什么样（labels、每机 runner 数）、用哪些池。
// 建机参数已移入池配置（§3.3）。
type Profile struct {
	Pools             []string `json:"pools"` // 有序列表（将来"本地优先、DO 溢出"预留；当前强制长度 1）
	Labels            []string `json:"labels"`
	RunnersPerDroplet int      `json:"runners_per_droplet"`
	ExtraPackages     []string `json:"extra_packages"` // 底座五包之外追加（REDESIGN §3.3）
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
	GitHubApp         GitHubAppCfg       `json:"github_app"`
	Pools             map[string]PoolCfg `json:"pools"`
	SSHPrivateKeyFile string             `json:"ssh_private_key_file"`
	SSHPublicKeyFiles []string           `json:"ssh_public_key_files"`
	MachineAgent      MachineAgentCfg    `json:"machine_agent"`
	Listen            string             `json:"listen"`
	StateDir          string             `json:"state_dir"`
	Global            GlobalCfg          `json:"global"`
	LLM               LLMCfg             `json:"llm"`
	MaintainerLogin   string             `json:"maintainer_login"`
	Profiles          map[string]Profile `json:"profiles"`
	Repos             []RepoEntry        `json:"repos"`
	TokenFile         string             `json:"token_file"` // 阶段 1-2 临时 token 源（App 认证前）
}

var (
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	labelRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	pkgRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
)

// Load 读取并校验配置（错误信息中文、指出字段路径）。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	if err := checkLegacyFields(raw); err != nil {
		return nil, fmt.Errorf("配置 %s 使用旧 schema: %w", path, err)
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

// checkLegacyFields 检出旧 schema 字段并给出迁移提示（不做兼容解析）。
func checkLegacyFields(raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil // 解析失败由后续 decode 报错
	}
	var hits []string
	if _, ok := top["vpsctl_bin"]; ok {
		hits = append(hits, "顶层 vpsctl_bin → pools.<池名>.vpsctl.bin")
	}
	if _, ok := top["vpsctl_accounts_file"]; ok {
		hits = append(hits, "顶层 vpsctl_accounts_file → pools.<池名>.vpsctl.accounts_file")
	}
	if g, ok := top["global"]; ok {
		var gm map[string]json.RawMessage
		if json.Unmarshal(g, &gm) == nil {
			if _, ok := gm["max_droplets"]; ok {
				hits = append(hits, "global.max_droplets → global.max_machines")
			}
		}
	}
	if ps, ok := top["profiles"]; ok {
		var pm map[string]map[string]json.RawMessage
		if json.Unmarshal(ps, &pm) == nil {
			legacyKeys := []string{"account", "region", "size", "image", "snapshot", "fallback_regions"}
			for name, fields := range pm {
				for _, k := range legacyKeys {
					if _, ok := fields[k]; ok {
						hits = append(hits, fmt.Sprintf("profiles.%s.%s → pools.<池名>.vpsctl.%s", name, k, k))
					}
				}
			}
		}
	}
	if len(hits) == 0 {
		return nil
	}
	return fmt.Errorf("检测到 %d 处旧字段，请按 docs/PROVIDER-ABSTRACTION.md §3.3 迁移：\n  - %s\n迁移要点：DO 建机参数（bin/accounts_file/account/region/size/image/snapshot）移入 pools.<池名>.vpsctl 子对象并补 hourly_cost_hint；profile 只留 pools/labels/runners_per_droplet（profile.pools 长度当前必须为 1）",
		len(hits), strings.Join(hits, "\n  - "))
}

func (c *Config) validate() error {
	// 认证源二选一（§3.4.2）：GitHub App 与 PAT token_file 同时配置即错
	appCfg := c.GitHubApp.AppID != 0 || c.GitHubApp.PrivateKeyFile != ""
	if appCfg && c.TokenFile != "" {
		return fmt.Errorf("github_app 与 token_file 只能二选一（App 为生产路径，token_file 为开发备选）")
	}

	if c.StateDir == "" {
		return fmt.Errorf("state_dir：必填（状态与 cloud-init 落盘的根目录）")
	}
	if len(c.Pools) == 0 {
		return fmt.Errorf("pools：至少配置一个机器池")
	}
	for name, p := range c.Pools {
		if name == "" {
			return fmt.Errorf("pools：池名不能为空")
		}
		if p.Provider == "" {
			return fmt.Errorf("pools.%s.provider：必填（已注册：%v）", name, machine.Registered())
		}
		if p.MaxMachines < 1 {
			return fmt.Errorf("pools.%s.max_machines：必须 ≥1", name)
		}
		// 用注册的 Factory 试构造一次：私有配置齐全性由实现校验（启动期报错）
		if _, err := machine.New(p.Provider, name, p.Raw, machine.Env{StateDir: c.StateDir}); err != nil {
			return err
		}
	}
	if len(c.Repos) == 0 {
		return fmt.Errorf("repos：至少配置一个仓（owner/name）")
	}
	if len(c.Profiles) == 0 {
		return fmt.Errorf("profiles：至少配置一个 profile")
	}
	for name, p := range c.Profiles {
		if len(p.Pools) == 0 {
			return fmt.Errorf("profiles.%s.pools：至少挂 1 个池", name)
		}
		if len(p.Pools) > 1 {
			return fmt.Errorf("profiles.%s.pools：长度 %d——多池调度尚未实现（schema 预留，当前只能挂 1 个池）", name, len(p.Pools))
		}
		for i, pn := range p.Pools {
			if _, ok := c.Pools[pn]; !ok {
				return fmt.Errorf("profiles.%s.pools[%d]：%q 不存在于 pools", name, i, pn)
			}
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
		for _, pkg := range p.ExtraPackages {
			if !pkgRe.MatchString(pkg) {
				return fmt.Errorf("profiles.%s.extra_packages：%q 不是合法包名", name, pkg)
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
	if c.Global.MaxMachines < maxRunners {
		return fmt.Errorf("global.max_machines（%d）：必须 ≥ 任一仓的 max_runners（最大 %d）", c.Global.MaxMachines, maxRunners)
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

// PoolNameOf 返回 profile 用的池名（校验强制长度 1）。
func (c *Config) PoolNameOf(p Profile) string {
	if len(p.Pools) == 0 {
		return ""
	}
	return p.Pools[0]
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

// Tag 返回仓的机器归属标记（§3.5：ciorch-r-<owner>-<name>-<sha256 前 8 位>，
// sanitize 后总长 ≤64——用 owner/name 各截一段保证）。
func (r RepoEntry) Tag() string {
	parts := strings.SplitN(r.Repo, "/", 2)
	owner, name := sanitize(parts[0], 20), sanitize(parts[1], 20)
	sum := sha256Prefix(r.Repo, 8)
	return fmt.Sprintf("ciorch-r-%s-%s-%s", owner, name, sum)
}

// NamePrefix 返回机器名前缀（vpsctl -name-prefix）：ci-<sanitize(name) 截 20>。
// 命名格式的其余部分由池的 Namer 决定（§3.2）。
func (r RepoEntry) NamePrefix() string {
	parts := strings.SplitN(r.Repo, "/", 2)
	return "ci-" + sanitize(parts[1], 20)
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
