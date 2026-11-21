package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// §8.1：JWT 用测试密钥签名后用公钥验签；claims 口径（iat/exp/iss）。
func TestSignJWTAndVerify(t *testing.T) {
	key := testKey(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	jwt, err := SignJWT(12345, key, now)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyJWTSignature(jwt, &key.PublicKey)
	if err != nil || !ok {
		t.Fatalf("公钥应能验签: %v %v", ok, err)
	}
	// 换一把钥匙验不过
	other := testKey(t)
	if ok2, _ := VerifyJWTSignature(jwt, &other.PublicKey); ok2 {
		t.Fatal("错误公钥不应验签通过")
	}
	claims, err := ParseJWTClaims(jwt)
	if err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != float64(12345) {
		t.Fatalf("iss 应为 app_id: %v", claims["iss"])
	}
	if int64(claims["iat"].(float64)) != now.Unix()-60 {
		t.Fatalf("iat 应 now-60s: %v", claims["iat"])
	}
	if int64(claims["exp"].(float64)) != now.Add(9*time.Minute).Unix() {
		t.Fatalf("exp 应 now+9m: %v", claims["exp"])
	}
}

// AppAuth：查 installation → 铸 token → 缓存（第二次零 HTTP）→ 过期前
// 5 分钟视为失效重铸。
func TestAppAuthTokenFlow(t *testing.T) {
	key := testKey(t)
	var installs, mints int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/a/b/installation":
			// JWT 验签（用公钥验证 Authorization 里的 JWT）
			if !verifyAuthJWT(t, r, key) {
				w.WriteHeader(401)
				return
			}
			atomic.AddInt32(&installs, 1)
			w.Write([]byte(`{"id":77}`))
		case r.URL.Path == "/app/installations/77/access_tokens":
			if !verifyAuthJWT(t, r, key) {
				w.WriteHeader(401)
				return
			}
			atomic.AddInt32(&mints, 1)
			w.Write([]byte(`{"token":"ghs_INSTALLTOKEN_12345","expires_at":"2026-11-21T13:00:00Z"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	a := NewAppAuth(12345, key)
	a.BaseURL = srv.URL
	a.Now = func() time.Time { return now }

	tok, err := a.Token(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_INSTALLTOKEN_12345" {
		t.Fatalf("token 不对: %s", tok)
	}
	// 缓存：再取不发 HTTP
	if _, err := a.Token(context.Background(), "a/b"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&installs) != 1 || atomic.LoadInt32(&mints) != 1 {
		t.Fatalf("缓存应零 HTTP: installs=%d mints=%d", installs, mints)
	}
	// 距过期 <5 分钟 → 重铸
	now = time.Date(2026, 11, 21, 12, 56, 0, 0, time.UTC) // 13:00 - 4m
	tok2, err := a.Token(context.Background(), "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&mints) != 2 || tok2 != tok {
		t.Fatalf("过期前 5 分钟应重铸: mints=%d", mints)
	}
}

func verifyAuthJWT(t *testing.T, r *http.Request, key *rsa.PrivateKey) bool {
	t.Helper()
	auth := r.Header.Get("Authorization")
	if len(auth) < 8 || auth[:7] != "Bearer " {
		return false
	}
	ok, err := VerifyJWTSignature(auth[7:], &key.PublicKey)
	return err == nil && ok
}

// PEM 往返：PKCS#1 与 PKCS#8 都能解析。
func TestParseRSAPrivateKeyFromPEM(t *testing.T) {
	key := testKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if _, err := ParseRSAPrivateKeyFromPEM(pkcs1); err != nil {
		t.Fatalf("PKCS#1: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := ParseRSAPrivateKeyFromPEM(pkcs8); err != nil {
		t.Fatalf("PKCS#8: %v", err)
	}
	if _, err := ParseRSAPrivateKeyFromPEM([]byte("not pem")); err == nil {
		t.Fatal("非 PEM 应报错")
	}
}

// Client App 路径：installation token 用于业务请求；401 作废重取一次。
func TestAppClient401Retries(t *testing.T) {
	key := testKey(t)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		tok := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/repos/a/b/installation":
			if !verifyAuthJWT(t, r, key) {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"id":77}`))
		case "/app/installations/77/access_tokens":
			w.Write([]byte(`{"token":"ghs_T` + string('A'+byte(n)) + `XXXXXXXXXXXX","expires_at":"2026-11-21T23:00:00Z"}`))
		case "/repos/a/b/actions/runners":
			if tok == "Bearer gs_STALE" && n <= 1 {
				w.WriteHeader(401) // 首次：缓存里的旧 token 失效
				return
			}
			w.Write([]byte(`{"runners":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := NewAppAuth(12345, key)
	a.BaseURL = srv.URL
	a.Now = func() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }
	// 预置一个失效 token 模拟缓存过期边界
	a.tokens["a/b"] = cachedToken{token: "gs_STALE", expiresAt: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}

	c := NewAppClient(a, WithBaseURL(srv.URL), WithRetryBackoff(0))
	zeroSleep(c)
	if _, err := c.ListRunners(context.Background(), "a/b"); err != nil {
		t.Fatalf("401 后应作废重取成功: %v", err)
	}
}
