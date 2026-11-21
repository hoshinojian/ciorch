// GitHub App 认证（REDESIGN §3.4.2）：RS256 JWT（标准库实现）、按仓查
// installation id（缓存）、installation token（缓存到过期前 5 分钟）。
// PAT / gh token 仍是开发备选（config 校验互斥，见 config.validate）。
package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// SignJWT 用 RS256 签发 GitHub App JWT（§3.4.2：iat=now-60s、exp=now+9min、
// iss=app_id；标准库 crypto 实现，不引第三方库）。
func SignJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := `{"alg":"RS256","typ":"JWT"}`
	claims := fmt.Sprintf(`{"iat":%d,"exp":%d,"iss":%d}`,
		now.Unix()-60, now.Add(9*time.Minute).Unix(), appID)
	signingInput := base64.RawURLEncoding.EncodeToString([]byte(header)) +
		"." + base64.RawURLEncoding.EncodeToString([]byte(claims))
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("JWT 签名: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// appTokenLead 是 token 缓存的安全提前量：过期前 5 分钟即视为失效（§3.4.2）。
const appTokenLead = 5 * time.Minute

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// AppAuth 按仓铸 installation token（JWT 签发 → 查 installation → 换 token）。
type AppAuth struct {
	AppID   int64
	Key     *rsa.PrivateKey
	BaseURL string // 默认 https://api.github.com
	HTTP    *http.Client
	Now     func() time.Time // 可注入（测试）

	mu       sync.Mutex
	installs map[string]int64       // repo → installation id（重启重查语义：进程内缓存）
	tokens   map[string]cachedToken // repo → token
}

// NewAppAuth 构造。
func NewAppAuth(appID int64, key *rsa.PrivateKey) *AppAuth {
	return &AppAuth{
		AppID: appID, Key: key,
		BaseURL:  defaultBaseURL,
		HTTP:     &http.Client{Timeout: 15 * time.Second},
		Now:      time.Now,
		installs: map[string]int64{},
		tokens:   map[string]cachedToken{},
	}
}

// Token 返回该仓的 installation token（缓存到过期前 5 分钟）。
func (a *AppAuth) Token(ctx context.Context, repo string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Now()
	if t, ok := a.tokens[repo]; ok && now.Before(t.expiresAt.Add(-appTokenLead)) {
		return t.token, nil
	}
	instID, err := a.installationIDLocked(ctx, repo)
	if err != nil {
		return "", err
	}
	var res struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.appCall(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", instID), &res); err != nil {
		return "", fmt.Errorf("铸 installation token: %w", err)
	}
	if res.Token == "" {
		return "", fmt.Errorf("installation token 为空")
	}
	a.tokens[repo] = cachedToken{token: res.Token, expiresAt: res.ExpiresAt}
	return res.Token, nil
}

// Invalidate 作废该仓 token 缓存（收到 401 时调用，随后重取一次）。
func (a *AppAuth) Invalidate(repo string) {
	a.mu.Lock()
	delete(a.tokens, repo)
	a.mu.Unlock()
}

func (a *AppAuth) installationIDLocked(ctx context.Context, repo string) (int64, error) {
	if id, ok := a.installs[repo]; ok {
		return id, nil
	}
	var res struct {
		ID int64 `json:"id"`
	}
	if err := a.appCall(ctx, http.MethodGet, "/repos/"+repo+"/installation", &res); err != nil {
		return 0, fmt.Errorf("查 installation: %w", err)
	}
	if res.ID == 0 {
		return 0, fmt.Errorf("仓 %s 未安装本 App", repo)
	}
	a.installs[repo] = res.ID
	return res.ID, nil
}

// appCall 以 App JWT 调 /app 前缀端点并解析 JSON。
func (a *AppAuth) appCall(ctx context.Context, method, path string, out any) error {
	jwt, err := SignJWT(a.AppID, a.Key, a.Now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "ciorch")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, truncateStr(body, 200))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

func truncateStr(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}

// ParseJWTClaims 解析（不验签）JWT 的 claims——测试与诊断用。
func ParseJWTClaims(jwt string) (map[string]any, error) {
	parts := splitJWT(jwt)
	if len(parts) != 3 {
		return nil, fmt.Errorf("JWT 应有 3 段")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// VerifyJWTSignature 用公钥验签（测试用：确认 SignJWT 产出可被 GitHub 方式验证）。
func VerifyJWTSignature(jwt string, pub *rsa.PublicKey) (bool, error) {
	parts := splitJWT(jwt)
	if len(parts) != 3 {
		return false, fmt.Errorf("JWT 应有 3 段")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return false, nil
	}
	return true, nil
}

func splitJWT(jwt string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(jwt); i++ {
		if jwt[i] == '.' {
			parts = append(parts, jwt[start:i])
			start = i + 1
		}
	}
	parts = append(parts, jwt[start:])
	return parts
}

// ParseRSAPrivateKeyFromPEM 用标准库解析 PEM 私钥（PKCS#1 "RSA PRIVATE KEY"
// 或 PKCS#8 "PRIVATE KEY"——GitHub App 下载的是 PKCS#1）。
func ParseRSAPrivateKeyFromPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("未找到 PEM 块")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析私钥: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("私钥不是 RSA")
	}
	return rk, nil
}
