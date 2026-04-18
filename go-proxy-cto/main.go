package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// subtleCompare 常量时间字符串比较，返回 1 表示相等
func subtleCompare(a, b string) int {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b))
}

var log *zap.Logger

func shortUUID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:20]
}

func main() {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stdout"}
	l, err := cfg.Build()
	if err != nil {
		panic(err)
	}
	log = l
	defer func() { _ = log.Sync() }()

	baseDir := os.Getenv("CTO_BASE_DIR")
	if baseDir == "" {
		exe, _ := os.Executable()
		baseDir = filepath.Dir(exe)
	}
	// 按优先级搜索 accounts.jsonl 所在目录
	searchDirs := []string{baseDir}
	if cwd, err := os.Getwd(); err == nil && cwd != baseDir {
		searchDirs = append(searchDirs, cwd)
	}
	// exe 的上级目录（二进制在 go-proxy-cto/ 子目录，accounts.jsonl 在项目根目录）
	if parent := filepath.Dir(baseDir); parent != baseDir {
		searchDirs = append(searchDirs, parent)
	}
	for _, d := range searchDirs {
		if _, err := os.Stat(filepath.Join(d, "accounts.jsonl")); err == nil {
			baseDir = d
			break
		}
	}
	log.Info("starting cto-proxy",
		zap.String("baseDir", baseDir),
		zap.String("listen", ListenAddr))

	pool := NewAccountPool(baseDir)
	pool.Load()

	poolStats := pool.Stats()
	log.Info("pool ready",
		zap.Any("total", poolStats["total"]),
		zap.Any("available", poolStats["available"]))

	if poolStats["total"].(int) == 0 {
		log.Warn("accounts.jsonl is empty; run cto_register.py to add accounts")
	}

	clerk := newClerkClient()
	engine := newEngineClient(clerk)

	go prefreshLoop(pool, clerk)
	go reloadLoop(pool)
	go persistLoop(pool)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// 鉴权中间件：必须匹配 APIKey（环境变量 PROXY_API_KEY 或默认值）
	// 修复历史漏洞：原实现只校验"非空"，任何字符串都能通过 —— 反代对公网开放
	// 就相当于裸奔。现在要求 token 完全等于 APIKey（安全前缀比对避免 timing 侧信道）。
	authMW := func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if len(auth) >= 7 && auth[:7] == "Bearer " {
			auth = auth[7:]
		}
		key := c.GetHeader("x-api-key")
		// 两个 header 取非空的那个
		got := auth
		if got == "" {
			got = key
		}
		if got == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"error": "API key required (Authorization: Bearer ... or x-api-key header)"})
			return
		}
		// 恒时比较，避免 timing attack
		if subtleCompare(got, APIKey) != 1 {
			log.Warn("auth: api key mismatch",
				zap.String("remote", c.ClientIP()),
				zap.Int("got_len", len(got)))
			c.AbortWithStatusJSON(http.StatusUnauthorized,
				gin.H{"error": "invalid API key"})
			return
		}
		c.Next()
	}

	startTime := time.Now()

	r.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(HomepageHTML))
	})

	r.GET("/status", func(c *gin.Context) {
		stats := pool.Stats()
		stats["uptime_sec"] = int(time.Since(startTime).Seconds())
		c.JSON(http.StatusOK, stats)
	})

	r.GET("/v1/models", func(c *gin.Context) {
		models := []map[string]string{}
		seen := map[string]bool{}
		for name := range ModelMap {
			if seen[name] {
				continue
			}
			seen[name] = true
			models = append(models, map[string]string{
				"id": name, "object": "model", "owned_by": "cto.new",
			})
		}
		c.JSON(http.StatusOK, gin.H{"object": "list", "data": models})
	})

	r.POST("/v1/chat/completions", authMW, func(c *gin.Context) {
		handleChatCompletions(c, pool, engine)
	})
	r.POST("/v1/messages", authMW, func(c *gin.Context) {
		handleAnthropicMessages(c, pool, engine)
	})

	// Admin endpoints —— 必须带 API key，否则账号邮箱/cookie 暴露
	r.GET("/admin/accounts", authMW, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"stats":    pool.Stats(),
			"accounts": pool.AccountList(),
		})
	})
	r.POST("/admin/reload", authMW, func(c *gin.Context) {
		before := len(pool.accounts)
		pool.Load()
		c.JSON(http.StatusOK, gin.H{
			"before": before, "after": len(pool.accounts),
		})
	})

	// 日志端点 —— 需要 API key
	r.GET("/admin/logs", authMW, func(c *gin.Context) {
		n := 100
		if q := c.Query("n"); q != "" {
			if v, err := strconv.Atoi(q); err == nil && v > 0 && v <= 500 {
				n = v
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"logs": globalLog.Recent(n),
		})
	})
	r.GET("/admin/log-stats", authMW, func(c *gin.Context) {
		c.JSON(http.StatusOK, globalLog.Stats())
	})

	srv := &http.Server{Addr: ListenAddr, Handler: r}
	go func() {
		fmt.Printf(`
+------------------------------------------------+
|  cto.new Reverse Proxy (Go)                    |
|  OpenAI:    POST /v1/chat/completions          |
|  Anthropic: POST /v1/messages                  |
|  Models:    gpt-5.4, glm-5.1 (+aliases)        |
|  Listen:    %-40s|
+------------------------------------------------+
`, ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("shutting down...")
	// 退出前强制把 dirty 的 cookie 持久化，防止丢失最新的 __client
	if n := pool.PersistDirty(); n > 0 {
		log.Info("shutdown: persisted dirty accounts", zap.Int("n", n))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown error", zap.Error(err))
	}
	log.Info("stopped")
}

// prefreshLoop 启动时刷新所有账号的 Clerk JWT，避免第一请求耗时
func prefreshLoop(pool *AccountPool, ck *clerkClient) {
	time.Sleep(2 * time.Second)
	pool.mu.RLock()
	accs := make([]*Account, len(pool.accounts))
	copy(accs, pool.accounts)
	pool.mu.RUnlock()

	ok, fail := 0, 0
	for _, a := range accs {
		if err := ck.refreshSession(a); err != nil {
			fail++
			log.Warn("prefresh fail",
				zap.String("email", a.Email),
				zap.Error(err))
			// Clerk 直接 401 → 标记失效
			if isAuthError(stringToLower(err.Error())) {
				a.MarkDead()
			}
			continue
		}
		ok++
		time.Sleep(300 * time.Millisecond)
	}
	log.Info("prefresh done",
		zap.Int("ok", ok), zap.Int("fail", fail),
		zap.Int("total", len(accs)))
}

// reloadLoop 周期性热加载新账号
func reloadLoop(pool *AccountPool) {
	for {
		time.Sleep(time.Duration(ReloadInterval) * time.Second)
		pool.ReloadIfNeeded()
	}
}

// persistLoop 周期性把被 Clerk rotate 过的 __client cookie 写回 accounts.jsonl。
// Clerk session rotation 机制：每次 refresh 会返回新的 rotating_token，
// 旧 __client 失效（反代重启或账号空闲后再刷会 401）。
// 10 秒扫一次，轻量；进程退出前也尽量刷一次。
func persistLoop(pool *AccountPool) {
	for {
		time.Sleep(10 * time.Second)
		pool.PersistDirty()
	}
}
