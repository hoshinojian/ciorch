package machine

import (
	"encoding/json"
	"strings"
	"testing"
)

// 测试用 fake factory（不依赖任何真实实现）。
func fakeFactory(pool string, raw json.RawMessage) (Provider, error) {
	return nil, nil
}

func TestRegisterAndNew(t *testing.T) {
	Register("testprov", fakeFactory)
	p, err := New("testprov", "pool-a", json.RawMessage(`{}`))
	if err != nil || p != nil {
		t.Fatalf("New 应返回 factory 的结果：p=%v err=%v", p, err)
	}
}

func TestNewUnknownKind(t *testing.T) {
	_, err := New("no-such-prov", "pool-a", nil)
	if err == nil || !strings.Contains(err.Error(), "未知 provider") {
		t.Fatalf("未注册 kind 应报错：err=%v", err)
	}
	if !strings.Contains(err.Error(), "testprov") {
		t.Fatalf("错误信息应列出已注册 kind 方便排错：err=%v", err)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("同 kind 重复注册应 panic（程序接线错误早暴露）")
		}
	}()
	Register("testprov", fakeFactory)
}

func TestRegisteredSorted(t *testing.T) {
	got := Registered()
	if len(got) < 1 || got[0] != "testprov" {
		t.Fatalf("Registered 应含 testprov：%v", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Registered 应排序：%v", got)
		}
	}
}
