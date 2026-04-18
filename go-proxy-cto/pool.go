package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// AccountPool 账号池：读 accounts.jsonl、热加载、分发、标记
type AccountPool struct {
	mu            sync.RWMutex
	accounts      []*Account
	knownEmails   map[string]bool
	baseDir       string
	accountsFile  string
	lastReload    time.Time
	totalSuccess  int64
	statsFile     string
}

func NewAccountPool(baseDir string) *AccountPool {
	return &AccountPool{
		knownEmails:  make(map[string]bool),
		baseDir:      baseDir,
		accountsFile: filepath.Join(baseDir, "accounts.jsonl"),
		statsFile:    filepath.Join(baseDir, "stats.json"),
	}
}

// Load 从 accounts.jsonl 加载所有账号到内存
func (p *AccountPool) Load() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadLocked()
}

func (p *AccountPool) loadLocked() {
	f, err := os.Open(p.accountsFile)
	if err != nil {
		log.Warn("pool: accounts file not found", zap.String("path", p.accountsFile))
		return
	}
	defer f.Close()

	loaded, skipped := 0, 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var a Account
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			skipped++
			continue
		}
		if a.Email == "" || p.knownEmails[a.Email] {
			continue
		}
		// 缺少 Clerk 会话信息的账号没法用
		if a.ClerkSessionID == "" || a.ClerkClientCookie == "" {
			log.Warn("pool: skip incomplete account (missing clerk_session_id / clerk_client_cookie)",
				zap.String("email", a.Email))
			skipped++
			continue
		}
		// ws_token_suffix 兜底（老账号可能没这个字段）
		if a.WSTokenSuffix == "" && a.ClerkUserID != "" && a.ClerkOrgID != "" {
			a.WSTokenSuffix = a.ClerkUserID + ":" + a.ClerkOrgID
		}
		p.accounts = append(p.accounts, &a)
		p.knownEmails[a.Email] = true
		loaded++
	}
	p.lastReload = time.Now()
	if loaded > 0 || skipped > 0 {
		log.Info("pool: load done",
			zap.Int("loaded", loaded),
			zap.Int("skipped", skipped),
			zap.Int("total", len(p.accounts)))
	}
}

// ReloadIfNeeded 周期性触发，累加读取新增账号
func (p *AccountPool) ReloadIfNeeded() {
	p.mu.RLock()
	stale := time.Since(p.lastReload) > time.Duration(ReloadInterval)*time.Second
	p.mu.RUnlock()
	if !stale {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	before := len(p.accounts)
	p.loadLocked()
	if delta := len(p.accounts) - before; delta > 0 {
		log.Info("pool: hot added", zap.Int("n", delta))
	}
}

// Get 分配一个可用账号（LRU）
func (p *AccountPool) Get() *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	cand := []*Account{}
	for _, a := range p.accounts {
		if a.IsAvailable() {
			cand = append(cand, a)
		}
	}
	if len(cand) == 0 {
		return nil
	}
	sort.Slice(cand, func(i, j int) bool {
		return cand[i].lastUsed.Before(cand[j].lastUsed)
	})
	acc := cand[0]
	acc.inUse = true
	acc.lastUsed = time.Now()
	return acc
}

// Release 归还账号，可附带错误信息用于自动标记
func (p *AccountPool) Release(a *Account, errMsg string) {
	if a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a.inUse = false
	if errMsg == "" {
		return
	}
	low := stringToLower(errMsg)
	if isQuotaError(low) {
		a.exhausted = true
		a.exhaustedAt = time.Now()
	}
	if isAuthError(low) {
		// Clerk 401/cookie 失效的情况
		a.dead = true
	}
}

// IncrementSuccess 累加成功计数
func (p *AccountPool) IncrementSuccess(a *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a != nil {
		a.successCount++
	}
	p.totalSuccess++
}

// Stats 简要统计
func (p *AccountPool) Stats() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var avail, dead, exhausted, inUse int
	for _, a := range p.accounts {
		if a.dead {
			dead++
		} else if a.exhausted {
			exhausted++
		} else if a.inUse {
			inUse++
		} else {
			avail++
		}
	}
	return map[string]interface{}{
		"status":        "ok",
		"total":         len(p.accounts),
		"available":     avail,
		"exhausted":     exhausted,
		"dead":          dead,
		"in_use":        inUse,
		"total_success": p.totalSuccess,
	}
}

func (p *AccountPool) AccountList() []map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]map[string]interface{}, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, map[string]interface{}{
			"email":     a.Email,
			"in_use":    a.inUse,
			"exhausted": a.exhausted,
			"dead":      a.dead,
			"success":   a.successCount,
			"errors":    a.errorCount,
		})
	}
	return out
}

// PersistDirty 把内存里被轮换过 cookie 的账号写回 accounts.jsonl。
// 采用"按邮箱匹配替换对应行"的增量方式，不触碰其它行，避免并发 append 时覆盖。
// 用临时文件 + rename 保证原子性。
func (p *AccountPool) PersistDirty() int {
	p.mu.Lock()
	dirty := []*Account{}
	for _, a := range p.accounts {
		a.mu.Lock()
		if a.IsDirty() {
			dirty = append(dirty, a)
		}
		a.mu.Unlock()
	}
	if len(dirty) == 0 {
		p.mu.Unlock()
		return 0
	}

	// 构造 email -> 最新 JSON 映射
	latest := make(map[string][]byte, len(dirty))
	for _, a := range dirty {
		a.mu.Lock()
		b, err := json.Marshal(a)
		a.mu.Unlock()
		if err == nil {
			latest[a.Email] = b
		}
	}
	p.mu.Unlock()

	// 读原文件 → 逐行替换匹配的 → 写临时 → rename
	src, err := os.Open(p.accountsFile)
	if err != nil {
		log.Warn("persist: open source failed", zap.Error(err))
		return 0
	}
	tmp, err := os.CreateTemp(p.baseDir, ".accounts.jsonl.tmp-*")
	if err != nil {
		src.Close()
		log.Warn("persist: create tmp failed", zap.Error(err))
		return 0
	}

	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	writer := bufio.NewWriter(tmp)
	replaced := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// 快速取出 email 字段，避免全部 unmarshal
		var row struct {
			Email string `json:"email"`
		}
		if err := json.Unmarshal(line, &row); err == nil {
			if newB, ok := latest[row.Email]; ok {
				writer.Write(newB)
				writer.WriteByte('\n')
				replaced++
				delete(latest, row.Email)
				continue
			}
		}
		writer.Write(line)
		writer.WriteByte('\n')
	}
	// 剩下的（原文件没有的新账号）也追加
	for _, b := range latest {
		writer.Write(b)
		writer.WriteByte('\n')
		replaced++
	}
	writer.Flush()
	src.Close()
	tmp.Close()

	if err := os.Rename(tmp.Name(), p.accountsFile); err != nil {
		log.Warn("persist: rename failed", zap.Error(err))
		os.Remove(tmp.Name())
		return 0
	}

	// 标记已持久化
	p.mu.Lock()
	for _, a := range dirty {
		a.mu.Lock()
		a.clearDirty()
		a.mu.Unlock()
	}
	p.mu.Unlock()

	log.Info("persist: rotated cookies written back", zap.Int("count", replaced))
	return replaced
}

// isQuotaError 文本包含配额关键字
func isQuotaError(msgLower string) bool {
	for _, kw := range QuotaErrorKeywords {
		if containsStr(msgLower, kw) {
			return true
		}
	}
	return false
}

// isAuthError 认证失败关键字
func isAuthError(msgLower string) bool {
	for _, kw := range []string{
		"401", "unauthorized", "unauthenticated", "jwt expired",
		"forbidden", "session not found",
	} {
		if containsStr(msgLower, kw) {
			return true
		}
	}
	return false
}
