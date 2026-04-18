package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// clerkClient 封装 Clerk 会话管理：
//   核心 endpoint: POST /v1/client/sessions/{sid}/tokens  用来把 __session cookie 换新 JWT
//   每个账号独立一个 http.Client（带自己的 cookie jar）
type clerkClient struct {
	http *http.Client
}

func newClerkClient() *clerkClient {
	return &clerkClient{
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

// jwtClaims 解析后的 Clerk JWT 关键字段（只用于判断过期）
type jwtClaims struct {
	Exp int64  `json:"exp"`
	Iat int64  `json:"iat"`
	Sub string `json:"sub"`
	Sid string `json:"sid"`
}

// parseJWTExp 不校验签名，只取 exp 字段（Clerk JWT 是 RS256 + base64url）
func parseJWTExp(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload := parts[1]
	// URL-safe base64 补齐
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	data, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return 0
	}
	var c jwtClaims
	if err := json.Unmarshal(data, &c); err != nil {
		return 0
	}
	return c.Exp
}

// refreshSession 用 __client cookie 向 Clerk 换一份新的短期 JWT。
// 若 cookie 已失效会返回 401，account 会被标记为 dead。
func (c *clerkClient) refreshSession(a *Account) error {
	if a.ClerkSessionID == "" || a.ClerkClientCookie == "" {
		return fmt.Errorf("clerk: missing session id / __client cookie for %s", a.Email)
	}

	u := fmt.Sprintf(
		"%s/v1/client/sessions/%s/tokens?__clerk_api_version=%s&_clerk_js_version=%s",
		ClerkAPI, a.ClerkSessionID,
		url.QueryEscape(ClerkAPIVer),
		url.QueryEscape(ClerkJSVer),
	)

	req, err := http.NewRequest("POST", u, strings.NewReader(""))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://cto.new")
	req.Header.Set("Referer", "https://cto.new/")
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	req.Header.Set("Cookie", buildClerkCookie(a))

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("clerk refresh: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("clerk refresh HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 200))
	}

	var out struct {
		JWT string `json:"jwt"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.JWT == "" {
		return fmt.Errorf("clerk refresh: no jwt in response: %s", truncate(string(body), 200))
	}

	a.mu.Lock()
	a.JWT = out.JWT
	a.JWTExp = parseJWTExp(out.JWT)

	// ⭐ 关键：Clerk 每次 refresh 都会在 Set-Cookie 里返回**新的** __client
	// （rotating_token 换新），旧的会在一段时间后失效。必须同步到内存 + 持久化，
	// 否则反代重启或账号空闲后再刷会 401 "Signed out"。
	clientRotated := false
	for _, setCookie := range resp.Header.Values("Set-Cookie") {
		switch {
		case strings.HasPrefix(setCookie, "__client="):
			val := strings.SplitN(setCookie, ";", 2)[0]
			val = strings.TrimPrefix(val, "__client=")
			if val != "" && val != a.ClerkClientCookie {
				a.ClerkClientCookie = val
				clientRotated = true
			}
		case strings.HasPrefix(setCookie, "__session="):
			val := strings.SplitN(setCookie, ";", 2)[0]
			val = strings.TrimPrefix(val, "__session=")
			a.ClerkCookie = val
		case strings.HasPrefix(setCookie, "__client_uat="):
			val := strings.SplitN(setCookie, ";", 2)[0]
			val = strings.TrimPrefix(val, "__client_uat=")
			a.ClerkClientUAT = val
		}
	}
	a.mu.Unlock()

	log.Debug("clerk refreshed",
		zap.String("email", a.Email),
		zap.Bool("client_rotated", clientRotated),
		zap.Int64("exp_in", a.JWTExp-time.Now().Unix()))

	// 有轮换就立刻落盘，避免反代崩溃/重启丢失
	if clientRotated {
		a.markDirty()
	}
	return nil
}

// buildClerkCookie 把账号里保存的 cookie 拼成请求头。
// Clerk 刷新 token 的关键 cookie 是 __client（HTTP-only），其余是辅助。
func buildClerkCookie(a *Account) string {
	parts := []string{}
	if a.ClerkClientCookie != "" {
		parts = append(parts, "__client="+a.ClerkClientCookie)
	}
	if a.JWT != "" {
		// 反代运行中优先用最新刷出来的 JWT
		parts = append(parts, "__session="+a.JWT)
	} else if a.ClerkCookie != "" {
		parts = append(parts, "__session="+a.ClerkCookie)
	}
	if a.ClerkClientUAT != "" {
		parts = append(parts, "__client_uat="+a.ClerkClientUAT)
	}
	return strings.Join(parts, "; ")
}

// EnsureFreshJWT 如果 JWT 还剩 < JWTRefreshTTL 秒，就去刷新。
// 反代每次处理请求前调用。
func (c *clerkClient) EnsureFreshJWT(a *Account) error {
	a.mu.Lock()
	exp := a.JWTExp
	token := a.JWT
	a.mu.Unlock()

	now := time.Now().Unix()
	if token != "" && exp > 0 && now < exp-int64(JWTRefreshTTL) {
		return nil
	}
	return c.refreshSession(a)
}

// ensureFreshJWTLocked 相同目的，但在调用处已持锁（避免递归锁）。
// 当前未使用，保留以备后用。
var _ = ensureFreshJWTLocked

func ensureFreshJWTLocked(a *Account, c *clerkClient) error {
	now := time.Now().Unix()
	if a.JWT != "" && a.JWTExp > 0 && now < a.JWTExp-int64(JWTRefreshTTL) {
		return nil
	}
	a.mu.Unlock()
	err := c.refreshSession(a)
	a.mu.Lock()
	return err
}

// 保持一个全局 mutex 保护 clerkClient（当前实现里 http.Client 并发安全，占位以防扩展）
var _ = sync.Mutex{}
