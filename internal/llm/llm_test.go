package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// §8.1 llm：请求体字段（JSON 模式、temperature 0、max_tokens、消息对）与请求头。
func TestCompleteRequestBody(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if r.URL.Path != "/chat/completions" {
			t.Errorf("路径：%s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"none\"}"}}]}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "deepseek-chat", APIKey: "sk-test"}
	out, err := c.Complete(context.Background(), SystemPrompt("A", []string{"none"}), `{"incident":{}}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"action":"none"}` {
		t.Fatalf("应返回 choices[0].message.content：%q", out)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization：%q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type：%q", gotCT)
	}
	if gotBody["model"] != "deepseek-chat" {
		t.Fatalf("model：%v", gotBody["model"])
	}
	if gotBody["temperature"] != float64(0) {
		t.Fatalf("temperature 应为 0：%v", gotBody["temperature"])
	}
	if rf, ok := gotBody["response_format"].(map[string]any); !ok || rf["type"] != "json_object" {
		t.Fatalf("response_format 应为 json_object：%v", gotBody["response_format"])
	}
	if gotBody["max_tokens"] != float64(2000) {
		t.Fatalf("max_tokens：%v", gotBody["max_tokens"])
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 应为 system+user 两条：%v", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	usr, _ := msgs[1].(map[string]any)
	if sys["role"] != "system" || usr["role"] != "user" {
		t.Fatalf("消息角色：%v %v", sys["role"], usr["role"])
	}
	if sys["content"] == "" || usr["content"] == "" {
		t.Fatal("消息内容不能为空")
	}
}

// §8.1 llm：超时报错（不吞成空响应）。
func TestCompleteTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m", APIKey: "k", HTTP: &http.Client{Timeout: 50 * time.Millisecond}}
	if _, err := c.Complete(context.Background(), "s json", "u"); err == nil {
		t.Fatal("超时应报错")
	}
}

// §8.1 llm：5xx 重试 1 次后成功；4xx 不重试。
func TestCompleteRetrySemantics(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Model: "m", APIKey: "k"}
	out, err := c.Complete(context.Background(), "s json", "u")
	if err != nil || out != "ok" {
		t.Fatalf("5xx 应重试后成功：%v %q", err, out)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("应恰好 2 次调用，got %d", got)
	}

	n.Store(0)
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv4.Close()
	c4 := &Client{BaseURL: srv4.URL, Model: "m", APIKey: "k"}
	if _, err := c4.Complete(context.Background(), "s json", "u"); err == nil {
		t.Fatal("401 应报错")
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("4xx 不应重试，got %d 次", got)
	}
}

// §8.1 llm：响应解析——无 choices / 带 error 字段都要报错。
func TestCompleteBadResponses(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"无 choices", `{}`},
		{"error 字段", `{"error":{"message":"quota"},"choices":[]}`},
		{"非 JSON", `not-json`},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(c.body))
		}))
		cl := &Client{BaseURL: srv.URL, Model: "m", APIKey: "k"}
		if _, err := cl.Complete(context.Background(), "s json", "u"); err == nil {
			t.Errorf("%s：应报错", c.name)
		}
		srv.Close()
	}
}

// SystemPrompt：含 "json" 字样（JSON 模式硬性要求）、白名单动作、场景代码。
func TestSystemPrompt(t *testing.T) {
	p := SystemPrompt("A", []string{"none", "restart_runner", "drain_droplet"})
	for _, want := range []string{"json", "诊断员", "P139", "P141", "restart_runner", "drain_droplet", "needs_human", "confidence"} {
		if !strings.Contains(p, want) {
			t.Errorf("SystemPrompt 应含 %q", want)
		}
	}
	// 不在白名单的动作不应出现（防止提示词给模型越权暗示）
	if strings.Contains(p, "switch_pool") {
		t.Error("A 类事故提示词不应出现 switch_pool")
	}
}
