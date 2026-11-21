// Registry：provider 类型名 → 构造函数（§3.1）。cmd 负责注册实现
// （import 提供商实现包），配置按池里的 "provider" 字段从这里构造。
package machine

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Factory 从池私有配置构造 Provider。raw 是池配置里与 provider 同名的
// 子对象（只有对应实现解析，其余字段由 config 层拒绝）；pool 是池名，
// 实现须把它写进 Machine.Pool。
type Factory func(pool string, raw json.RawMessage) (Provider, error)

var (
	regMu sync.RWMutex
	reg   = map[string]Factory{}
)

// Register 注册一种提供商实现（同 kind 重复注册视为程序错误，panic 早暴露）。
func Register(kind string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := reg[kind]; dup {
		panic(fmt.Sprintf("machine: provider %q 重复注册", kind))
	}
	reg[kind] = f
}

// New 按类型名构造 Provider（未注册的 kind 返回错误，配置校验依赖这一点）。
func New(kind, pool string, raw json.RawMessage) (Provider, error) {
	regMu.RLock()
	f, ok := reg[kind]
	regMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("未知 provider %q（已注册：%v）", kind, Registered())
	}
	return f(pool, raw)
}

// Registered 返回已注册的 provider 类型名（排序，错误信息/测试用）。
func Registered() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(reg))
	for k := range reg {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
