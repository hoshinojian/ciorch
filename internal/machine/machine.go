// Package machine 定义提供商无关的机器模型与 Provider 接口
// （docs/PROVIDER-ABSTRACTION.md §3.1/§3.2）。"机器从哪来"由各池的
// Provider 实现回答；业务包（observe/decide/engine/budget）只依赖本包，
// 不感知具体提供商（vpsctl/DO 或将来的自建虚拟化）。
//
// 依赖方向：本包不依赖任何业务包；提供商实现包只依赖本包。
package machine

import (
	"context"
	"time"
)

// Machine 是提供商无关的机器视图（替代 observe.Droplet / vpsctlapi.Droplet
// 在上层的使用）。List/Create 返回的 Machine 必须带非空 Pool（实现自己的池名）。
type Machine struct {
	Pool       string    // 所属池名（配置里的 key）
	ID         string    // 提供商内唯一 ID
	Name       string    // = runner 名
	CreatedAt  time.Time // 冷启/寿命/成本计时起点
	Addr       string    // 编排器 ssh 可达的地址（提供商决定私网还是公网）；可空
	HourlyCost float64   // USD/小时；自有机器为 0
	Tags       []string  // 归属标记（公共标记 + 仓标记）
}

// CreateRequest 是建机请求。名字由 ciorch 先定好（= runner 名，cloud-init
// 里已写死），提供商必须原样使用这个名字，做不到就返回错误。
type CreateRequest struct {
	Name     string
	Tags     []string
	UserData []byte // 渲染好的 cloud-init
}

// Provider 是一个池背后的机器来源。实现必须并发安全。
//
// 成本口径（§3.4）：Machine.HourlyCost 由提供商给出（vpsctl 按
// price_monthly/672）；空池建新机的预估价不走本接口，由池配置
// hourly_cost_hint 提供（PR ④ 落地；取舍理由见实施计划 PR ① 描述）。
type Provider interface {
	// Kind 返回实现类型（如 "vpsctl"），用于日志与 status。
	Kind() string
	// Create 建一台机器并立即返回（不等就绪；就绪以 GitHub 侧 runner online
	// 为准）。返回的 Machine 至少要有 ID、Name、CreatedAt。
	Create(ctx context.Context, req CreateRequest) (Machine, error)
	// Delete 删机。机器已不存在时返回 nil（幂等）。
	Delete(ctx context.Context, id string) error
	// List 列出带指定标记的全部机器。任何部分失败都必须返回 error，
	// 不得返回不完整的列表（observe 的"观察冻结"依赖这一点）。
	List(ctx context.Context, tag string) ([]Machine, error)
	// Namer 返回本池的命名规则。
	Namer() Namer
}

// Namer 是一个池的机器命名规则（§3.2）。vpsctl 的命名格式
// （{prefix}-{account}-{region}-NN）是它自己的约束，不是 ciorch 的，
// 由各实现提供；上层（config/decide）不再写死。
type Namer interface {
	// Next 在现有机器名里取下一个名字（跳号语义：同名空间最大序号 +1，
	// 被删机器的序号不回收）。existing 是本仓观察到的全部机器名。
	Next(prefix string, existing []string) string
	// Pattern 返回本池里"属于这个前缀"的名字正则（替代旧的
	// RepoEntry.NamespacePattern）。rpd=2 时 runner 名会带 -a/-b 后缀，
	// Pattern 只描述机器名，后缀匹配仍在 decide 里做。
	Pattern(prefix string) string
}
