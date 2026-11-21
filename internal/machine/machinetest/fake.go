// Fake 是内存版 Provider（引擎/守护等业务包单测用；不是任何真实提供商的模拟）。
package machinetest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hoshinojian/ciorch/internal/machine"
)

// Fake 实现必须并发安全（契约套件在 -race 下并发调用）。
type Fake struct {
	Pool string
	Now  func() time.Time
	// NamerImpl 可覆盖默认命名规则（默认 staticNamer：{prefix}-NN 递增）。
	NamerImpl machine.Namer

	mu       sync.Mutex
	nextID   int
	byID     map[string]machine.Machine
	failList bool

	// CreateErr 非空时 Create 直接失败（退避/事故路径测试；非并发场景使用）。
	CreateErr error

	// 记录调用（断言用）
	Created    []machine.CreateRequest
	Deleted    []string
	Events     []string // 动作时序（"create:<名>" / "delete:<id>"——顺序断言用）
	ListedTags []string // List 收到的 tag 参数
}

// NewFake 构造属于池 pool 的 fake。
func NewFake(pool string) *Fake {
	return &Fake{Pool: pool, byID: map[string]machine.Machine{}, Now: time.Now}
}

// Kind 实现 machine.Provider。
func (f *Fake) Kind() string { return "fake" }

// Create 记录请求并落一台内存机器（名字原样、生成唯一 ID）。
func (f *Fake) Create(ctx context.Context, req machine.CreateRequest) (machine.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateErr != nil {
		return machine.Machine{}, f.CreateErr
	}
	f.nextID++
	m := machine.Machine{
		Pool: f.Pool, ID: fmt.Sprintf("fake-%s-%d", f.Pool, f.nextID),
		Name: req.Name, CreatedAt: f.Now(),
		Tags: append([]string(nil), req.Tags...),
	}
	f.byID[m.ID] = m
	f.Created = append(f.Created, machine.CreateRequest{
		Name: req.Name, Tags: append([]string(nil), req.Tags...),
		UserData: append([]byte(nil), req.UserData...),
	})
	f.Events = append(f.Events, "create:"+req.Name)
	return m, nil
}

// Delete 删除（幂等：不存在也返回 nil）。
func (f *Fake) Delete(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byID, id)
	f.Deleted = append(f.Deleted, id)
	f.Events = append(f.Events, "delete:"+id)
	return nil
}

// Seed 预置一台机器（观察/预算测试用；Pool 由 Fake 补上）。
func (f *Fake) Seed(m machine.Machine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.Pool == "" {
		m.Pool = f.Pool
	}
	if m.ID == "" {
		f.nextID++
		m.ID = fmt.Sprintf("fake-%s-%d", f.Pool, f.nextID)
	}
	f.byID[m.ID] = m
}

// List 按标记精确过滤；FailNextList 注入后下一次返回 error。
func (f *Fake) List(ctx context.Context, tag string) ([]machine.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ListedTags = append(f.ListedTags, tag)
	if f.failList {
		f.failList = false
		return nil, fmt.Errorf("fake: 注入的 List 失败（部分账号不可用）")
	}
	var out []machine.Machine
	for _, m := range f.byID {
		for _, tg := range m.Tags {
			if tg == tag {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

// Namer 返回默认（或覆盖过的）命名规则。
func (f *Fake) Namer() machine.Namer {
	if f.NamerImpl != nil {
		return f.NamerImpl
	}
	return staticNamer{}
}

// FailNextList 注入下一次 List 失败（契约套件用例 5）。
func (f *Fake) FailNextList() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failList = true
}

// ByID 返回内部机器快照（测试断言用）。
func (f *Fake) ByID() map[string]machine.Machine {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]machine.Machine, len(f.byID))
	for k, v := range f.byID {
		out[k] = v
	}
	return out
}

var _ machine.Provider = (*Fake)(nil)
