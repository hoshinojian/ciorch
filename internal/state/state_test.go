package state

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type payload struct {
	A int    `json:"a"`
	B string `json:"b"`
}

func TestSaveLoadRoundtripAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.json")
	if err := Save(path, payload{A: 1, B: "x"}); err != nil {
		t.Fatal(err)
	}
	var got payload
	ok, err := Load(path, &got)
	if err != nil || !ok {
		t.Fatalf("load: ok=%v err=%v", ok, err)
	}
	if got.A != 1 || got.B != "x" {
		t.Fatalf("往返不一致: %+v", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("文件应 0600: %v", fi.Mode())
	}
	// 无 tmp 残留（原子写完成）
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp 应已 rename")
	}
}

func TestLoadMissingIsNew(t *testing.T) {
	var got payload
	ok, err := Load(filepath.Join(t.TempDir(), "none.json"), &got)
	if err != nil || ok {
		t.Fatalf("缺文件应视为全新: ok=%v err=%v", ok, err)
	}
}

func TestAppendLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incidents.jsonl")
	if err := AppendLine(path, `{"t":1}`); err != nil {
		t.Fatal(err)
	}
	if err := AppendLine(path, `{"t":2}`); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if len(strings.Split(strings.TrimSpace(string(raw)), "\n")) != 2 {
		t.Fatalf("应两行: %q", raw)
	}
}

func TestLockExcludesSecond(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ciorch.lock")
	unlock, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(path); err == nil {
		t.Fatal("第二把锁应失败（防双开）")
	}
	unlock()
	if _, err := Lock(path); err != nil {
		t.Fatalf("释放后应可再取: %v", err)
	}
}

// 并发 Save 不产生交错损坏（原子写）。
func TestSaveConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = Save(path, payload{A: i, B: "same"})
		}(i)
	}
	wg.Wait()
	var got payload
	if _, err := Load(path, &got); err != nil {
		t.Fatalf("并发写后应可解析: %v", err)
	}
	if got.B != "same" {
		t.Fatalf("内容不对: %+v", got)
	}
}
