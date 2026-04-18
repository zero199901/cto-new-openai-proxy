package main

import (
	"os"
	"strings"
)

// ============================================================
// 运行时常量
// ============================================================
const (
	ListenAddr     = "127.0.0.1:9091"              // 反代监听地址
	ClerkAPI       = "https://clerk.cto.new"       // Clerk 认证后端
	EngineAPI      = "https://api.enginelabs.ai"   // Engine Labs REST API
	EngineWS       = "wss://api.enginelabs.ai"     // Engine Labs WebSocket
	ClerkAPIVer    = "2025-11-10"                  // Clerk API 版本
	ClerkJSVer     = "6.7.3"                       // Clerk JS SDK 版本
	MaxRetries     = 6                             // 单次请求最大重试次数
	WSTimeout      = 180                           // WS 最长等待秒
	ReloadInterval = 30                            // 账号热加载间隔秒
	PurgeInterval  = 300                           // 过期账号清理间隔秒
	JWTRefreshTTL  = 50                            // JWT 剩余多少秒以内去刷新
)

// 对外 API Key（客户端 Authorization Bearer 传入的值）
var APIKey = envOr("PROXY_API_KEY", "xs2-cto-secret")

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ============================================================
// 模型映射 —— OpenAI / Anthropic 名称 → cto.new engineAgentKey
// 以下 engineAgentKey 来自实际抓的 /chat/adapters 响应。
// ============================================================

// ChatAdapter 描述一个 cto.new 模型适配器。
type ChatAdapter struct {
	EngineKey       string // 发给 /teams/preferred-model 的 key（下划线小写形式，如 "gpt_5_4"）
	EngineAgentKey  string // 内部 engineAgentKey（PascalCase，如 "GPT5_4"）
	DisplayLabel    string
	Provider        string // openai / google / anthropic / openrouter
	SupportsImages  bool
	UsageMultiplier int // 每次调用对配额的倍数
	MinimumTier     int // 0=免费, 1/2=付费
}

// 默认模型（OpenAI 别名没有匹配时的 fallback）
const DefaultEngineKey = "gpt_5_4"

// ModelMap: 接受的请求模型名 → ChatAdapter
// 尽量兼容 OpenAI、Anthropic、Google 官方命名。
var ModelMap = map[string]ChatAdapter{
	// -------- GPT 5.4 (免费可用, ×3) --------
	"gpt-5.4":          {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"gpt-5-4":          {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"gpt-5":            {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"gpt-4o":           {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"gpt-4":            {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"gpt-4-turbo":      {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"gpt-4o-mini":      {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},

	// -------- GLM 5.1 (免费, ×2) --------
	"glm-5.1":   {EngineKey: "glm_5_1", EngineAgentKey: "GLM5_1", DisplayLabel: "GLM 5.1", Provider: "openrouter", SupportsImages: false, UsageMultiplier: 2, MinimumTier: 0},
	"glm-5-1":   {EngineKey: "glm_5_1", EngineAgentKey: "GLM5_1", DisplayLabel: "GLM 5.1", Provider: "openrouter", SupportsImages: false, UsageMultiplier: 2, MinimumTier: 0},
	"glm-5":     {EngineKey: "glm_5_1", EngineAgentKey: "GLM5_1", DisplayLabel: "GLM 5.1", Provider: "openrouter", SupportsImages: false, UsageMultiplier: 2, MinimumTier: 0},
	"z-ai/glm-5.1": {EngineKey: "glm_5_1", EngineAgentKey: "GLM5_1", DisplayLabel: "GLM 5.1", Provider: "openrouter", SupportsImages: false, UsageMultiplier: 2, MinimumTier: 0},

	// -------- 其它常见别名（都路由到 GPT 5.4，能节省账号配额） --------
	"claude-sonnet-4":   {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"claude-sonnet-4.5": {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"claude-sonnet-4.6": {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"o1":                {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
	"o3":                {EngineKey: "gpt_5_4", EngineAgentKey: "GPT5_4", DisplayLabel: "GPT 5.4", Provider: "openai", SupportsImages: true, UsageMultiplier: 3, MinimumTier: 0},
}

// ResolveModel 把对外请求的模型名转成 cto.new 的适配器。
// 匹配顺序：精确匹配 → 前缀匹配（处理 SDK 加日期后缀，如 claude-3-5-sonnet-20241022）
// → 小写化 → fallback 到 GPT 5.4。
func ResolveModel(name string) ChatAdapter {
	if ad, ok := ModelMap[name]; ok {
		return ad
	}
	// 小写再查一次
	low := strings.ToLower(name)
	if ad, ok := ModelMap[low]; ok {
		return ad
	}
	// 前缀匹配：SDK 经常在模型名后追加日期/版本后缀
	// 如 "gpt-4o-2024-08-06"、"claude-sonnet-4-20250514"
	for k, ad := range ModelMap {
		if strings.HasPrefix(low, k+"-") || strings.HasPrefix(low, k+"_") {
			return ad
		}
	}
	// 未知模型时走 GPT 5.4（最稳、支持图像）
	return ModelMap["gpt-5.4"]
}

// ============================================================
// 可识别为配额错误的关键词（从原 anything 项目沿用并扩展）
// ============================================================
var QuotaErrorKeywords = []string{
	"limit", "quota", "rate", "exceeded", "too many", "throttl",
	"capacity", "free_limit", "usage_limit", "rate_limit",
	"insufficient", "not allowed", "daily limit", "weekly limit",
}
