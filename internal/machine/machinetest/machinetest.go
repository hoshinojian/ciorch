// Package machinetest 是 Provider 的契约测试套件（§6）：所有现在和将来的
// 提供商实现都必须通过本套件，保证业务包面对的行为一致。
// 用法：实现的测试里 machinetest.Run(t, func(t *testing.T) machine.Provider { ... })。
package machinetest

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/hoshinojian/ciorch/internal/machine"
)

// listFailer 由能注入 List 失败的实现可选实现（契约用例"部分失败必须返回
// error"需要从外部制造失败；不实现则该用例跳过并记录）。
type listFailer interface {
	FailNextList()
}

// Run 对一个新构造的 Provider 跑全部契约用例。new 每次返回一个干净实例。
// 建机名一律取自 p.Namer().Next（引擎的真实用法：名字由 ciorch 经命名规则
// 定好、提供商必须原样使用——各实现的命名格式不同，套件不写死名字）。
func Run(t *testing.T, new func(t *testing.T) machine.Provider) {
	t.Helper()
	t.Run("CreateEchoesNameAndTags", func(t *testing.T) {
		p := new(t)
		ctx := context.Background()
		name := p.Namer().Next("ci-demo", nil)
		m, err := p.Create(ctx, machine.CreateRequest{
			Name: name, Tags: []string{"ciorch", "ciorch-r-demo"},
			UserData: []byte("#cloud-config\n"),
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if m.Name != name {
			t.Fatalf("名字必须原样返回：got %q want %q", m.Name, name)
		}
		if m.ID == "" {
			t.Fatal("返回的 Machine 必须带 ID")
		}
		if m.CreatedAt.IsZero() {
			t.Fatal("返回的 Machine 必须带 CreatedAt")
		}
		ms, err := p.List(ctx, "ciorch-r-demo")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(ms) != 1 || ms[0].Name != name || ms[0].ID != m.ID {
			t.Fatalf("建机后应能按标记列出且名字原样：%+v", ms)
		}
		if ms[0].Pool == "" {
			t.Fatal("List 返回的 Machine 必须带所属池名")
		}
	})
	t.Run("ListFiltersByTag", func(t *testing.T) {
		p := new(t)
		ctx := context.Background()
		if _, err := p.Create(ctx, machine.CreateRequest{Name: p.Namer().Next("ci-demo", nil), Tags: []string{"ciorch", "ciorch-r-demo"}, UserData: []byte("#cloud-config\n")}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		ms, err := p.List(ctx, "ciorch-r-other")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(ms) != 0 {
			t.Fatalf("不带该标记的机器不应出现：%+v", ms)
		}
	})
	t.Run("DeleteRemovesFromList", func(t *testing.T) {
		p := new(t)
		ctx := context.Background()
		m, err := p.Create(ctx, machine.CreateRequest{Name: p.Namer().Next("ci-demo", nil), Tags: []string{"ciorch"}, UserData: []byte("#cloud-config\n")})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := p.Delete(ctx, m.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		ms, err := p.List(ctx, "ciorch")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(ms) != 0 {
			t.Fatalf("删机后应列不出：%+v", ms)
		}
	})
	t.Run("DeleteIdempotent", func(t *testing.T) {
		p := new(t)
		ctx := context.Background()
		m, err := p.Create(ctx, machine.CreateRequest{Name: p.Namer().Next("ci-demo", nil), Tags: []string{"ciorch"}, UserData: []byte("#cloud-config\n")})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := p.Delete(ctx, m.ID); err != nil {
			t.Fatalf("第一次 Delete: %v", err)
		}
		if err := p.Delete(ctx, m.ID); err != nil {
			t.Fatalf("重复删除必须返回 nil（幂等）：%v", err)
		}
	})
	t.Run("ListPartialFailureReturnsError", func(t *testing.T) {
		p := new(t)
		inj, ok := p.(listFailer)
		if !ok {
			t.Skipf("%T 不支持失败注入（未实现 FailNextList），跳过部分失败用例", p)
		}
		ctx := context.Background()
		if _, err := p.Create(ctx, machine.CreateRequest{Name: p.Namer().Next("ci-demo", nil), Tags: []string{"ciorch"}, UserData: []byte("#cloud-config\n")}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		inj.FailNextList()
		if ms, err := p.List(ctx, "ciorch"); err == nil {
			t.Fatalf("List 遇到失败必须返回 error，不得返回不完整列表：%+v", ms)
		}
	})
	t.Run("ConcurrentCreateList", func(t *testing.T) {
		p := new(t)
		ctx := context.Background()
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					// 前缀全局唯一（各实现的命名格式不同，名字经 Namer 取）
					name := p.Namer().Next(fmt.Sprintf("ci-g%d-%02d", g, i), nil)
					if _, err := p.Create(ctx, machine.CreateRequest{Name: name, Tags: []string{"ciorch"}, UserData: []byte("#cloud-config\n")}); err != nil {
						t.Errorf("并发 Create: %v", err)
						return
					}
					if _, err := p.List(ctx, "ciorch"); err != nil {
						t.Errorf("并发 List: %v", err)
						return
					}
				}
			}(g)
		}
		wg.Wait()
	})
}

// staticNamer 是 Fake 的默认命名规则：{prefix}-01 递增（仅测试用；
// 真实实现的命名规则由其 Namer 提供）。
type staticNamer struct{}

func (staticNamer) Next(prefix string, existing []string) string {
	max := 0
	for _, name := range existing {
		var n int
		if _, err := fmt.Sscanf(name, prefix+"-%d", &n); err == nil && n > max {
			max = n
		}
	}
	return fmt.Sprintf("%s-%02d", prefix, max+1)
}

func (staticNamer) Pattern(prefix string) string { return "^" + prefix + "-[0-9]+$" }
