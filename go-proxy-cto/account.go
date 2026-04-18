package main

import (
	"sync"
	"time"
)

// Account 一个 cto.new 账号的运行时状态 + 持久化字段
// 持久化字段和 cto_register.py 生成的 accounts.jsonl 对齐
type Account struct {
	// 持久化字段 —— 从 accounts.jsonl 读入
	Email              string `json:"email"`
	Password           string `json:"password"`
	ClerkUserID        string `json:"clerk_user_id"`
	ClerkSessionID     string `json:"clerk_session_id"`
	ClerkOrgID         string `json:"clerk_org_id"`
	ClerkClientCookie  string `json:"clerk_client_cookie"`  // __client cookie (HTTP-only, 唯一真正用于刷新的)
	ClerkCookie        string `json:"clerk_session_cookie"` // __session (短期 JWT, 可选, 启动时用于打底)
	ClerkClientUAT     string `json:"clerk_client_uat"`
	EngineUserID       string `json:"engine_user_id"`
	EngineTeamID       string `json:"engine_team_id"`
	EngineAPIKey       string `json:"engine_api_key"`
	DefaultWorkspaceID string `json:"default_workspace_id"`
	WSTokenSuffix      string `json:"ws_token_suffix"`

	// 运行时状态
	mu           sync.Mutex
	JWT          string // 最近一次刷新的 Clerk JWT
	JWTExp       int64  // JWT 到期时间 (unix)
	lastUsed     time.Time
	inUse        bool
	dead         bool      // cookie 彻底失效（Clerk 返回 401 多次）
	exhausted    bool      // 今日配额已耗尽
	exhaustedAt  time.Time
	successCount int64
	errorCount   int64

	// cookie 被轮换时置 true，由 pool 定时持久化器写回 accounts.jsonl
	dirty bool
}

// markDirty 在持久化字段（尤其 __client cookie）被修改后调用
// 注意：必须已持锁或不持锁（单字段写入 race 风险极低，可接受）
func (a *Account) markDirty() {
	a.dirty = true
}

// clearDirty persist 完成后清标
func (a *Account) clearDirty() {
	a.dirty = false
}

// IsDirty 是否需要持久化
func (a *Account) IsDirty() bool { return a.dirty }

// IsAvailable 是否可以分配出去给新请求
func (a *Account) IsAvailable() bool {
	if a.dead || a.inUse {
		return false
	}
	if a.exhausted {
		// 免费号每天 300/每周 900，简单起见 6h 冷却后再试
		if time.Since(a.exhaustedAt) < 6*time.Hour {
			return false
		}
		a.exhausted = false
	}
	return true
}

// MarkExhausted 标记此账号本周期用尽配额
func (a *Account) MarkExhausted() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.exhausted = true
	a.exhaustedAt = time.Now()
}

// MarkDead 标记此账号永久失效（注册机下次跑的时候会替换）
func (a *Account) MarkDead() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dead = true
}

// truncate 安全截断字符串，用于日志打印
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// stringToLower 无依赖的小写
func stringToLower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		b[i] = c
	}
	return string(b)
}

// containsStr 子串判断
func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
