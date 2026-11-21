package machinetest

import (
	"testing"

	"github.com/hoshinojian/ciorch/internal/machine"
)

// 契约套件自证：fake 必须通过全部用例（真实实现的套件在各实现包内跑）。
func TestFakePassesContract(t *testing.T) {
	Run(t, func(t *testing.T) machine.Provider { return NewFake("test-pool") })
}

// Fake 的 Pool 标记：建出的机器带自己的池名。
func TestFakeStampsPool(t *testing.T) {
	f := NewFake("my-pool")
	m, err := f.Create(nil, machine.CreateRequest{Name: "ci-x-01", Tags: []string{"ciorch"}})
	if err != nil {
		t.Fatal(err)
	}
	if m.Pool != "my-pool" {
		t.Fatalf("Machine.Pool 应为构造时的池名：got %q", m.Pool)
	}
}

// Fake 的默认命名：{prefix}-NN 跳号（与 staticNamer 语义一致）。
func TestFakeDefaultNamer(t *testing.T) {
	f := NewFake("my-pool")
	n := f.Namer()
	got := n.Next("ci-x", []string{"ci-x-01", "ci-x-03", "other-07"})
	if got != "ci-x-04" {
		t.Fatalf("跳号语义（最大序号+1）：got %q", got)
	}
	if pat := n.Pattern("ci-x"); pat != "^ci-x-[0-9]+$" {
		t.Fatalf("Pattern：got %q", pat)
	}
}
