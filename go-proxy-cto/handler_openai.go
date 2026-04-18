package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// handleChatCompletions 实现 OpenAI 兼容的 POST /v1/chat/completions
func handleChatCompletions(c *gin.Context, pool *AccountPool, engine *engineClient) {
	start := time.Now()
	var body struct {
		Model     string                   `json:"model"`
		Messages  []map[string]interface{} `json:"messages"`
		Stream    bool                     `json:"stream"`
		MaxTokens int                      `json:"max_tokens"`
		Tools     []map[string]interface{} `json:"tools"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
		return
	}
	if len(body.Messages) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "messages is required"})
		return
	}
	adapter := ResolveModel(body.Model)
	log.Info("openai: new request",
		zap.String("model", body.Model),
		zap.String("engineKey", adapter.EngineKey),
		zap.Bool("stream", body.Stream),
		zap.Int("msgs", len(body.Messages)))

	var lastErr string
	var usedEmail string
	var httpStatus int
	for retry := 0; retry < MaxRetries; retry++ {
		acc := pool.Get()
		if acc == nil {
			httpStatus = http.StatusServiceUnavailable
			c.JSON(httpStatus, gin.H{
				"error": "No available accounts (all exhausted or dead)",
			})
			globalLog.Add(LogEntry{
				Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/chat/completions",
				Model: body.Model, Account: "", Status: httpStatus,
				Duration: time.Since(start).Milliseconds(), Stream: body.Stream,
				Error: "no_available_accounts", RemoteIP: c.ClientIP(),
			})
			return
		}
		usedEmail = acc.Email

		if body.Stream {
			ch, err := SendChatAndStreamWithTools(c.Request.Context(), engine, acc,
				body.Messages, adapter, nil, body.Tools)
			if err != nil {
				lastErr = err.Error()
				// 413 消息过大，重试无意义，直接返回错误
				if strings.Contains(lastErr, "413") || strings.Contains(lastErr, "Message too large") {
					pool.Release(acc, "")
					c.JSON(http.StatusRequestEntityTooLarge, gin.H{
						"error": map[string]interface{}{
							"message": "Prompt too large for upstream API (413). Reduce message count or length.",
							"type":    "invalid_request_error",
							"code":    "message_too_large",
						},
					})
					globalLog.Add(LogEntry{
						Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/chat/completions",
						Model: body.Model, Account: usedEmail, Status: 413,
						Duration: time.Since(start).Milliseconds(), Stream: body.Stream,
						Error: "message_too_large", RemoteIP: c.ClientIP(),
					})
					return
				}
				log.Warn("openai: retry",
					zap.String("email", acc.Email),
					zap.Int("attempt", retry+1),
					zap.Error(err))
				pool.Release(acc, err.Error())
				continue
			}
			streamOpenAI(c, pool, acc, ch, body.Model, start, c.ClientIP())
			return
		}

		// 非 stream：累积所有 delta 再一次性返回
		result, err := nonStreamOpenAI(c.Request.Context(), engine, pool, acc,
			body.Messages, adapter, body.Model, body.Tools)
		if err != nil {
			lastErr = err.Error()
			// 413 消息过大，直接返回
			if strings.Contains(lastErr, "413") || strings.Contains(lastErr, "Message too large") {
				pool.Release(acc, "")
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{
					"error": map[string]interface{}{
						"message": "Prompt too large for upstream API (413). Reduce message count or length.",
						"type":    "invalid_request_error",
						"code":    "message_too_large",
					},
				})
				globalLog.Add(LogEntry{
					Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/chat/completions",
					Model: body.Model, Account: usedEmail, Status: 413,
					Duration: time.Since(start).Milliseconds(), Stream: false,
					Error: "message_too_large", RemoteIP: c.ClientIP(),
				})
				return
			}
			pool.Release(acc, err.Error())
			log.Warn("openai: nonstream retry",
				zap.String("email", acc.Email),
				zap.Int("attempt", retry+1),
				zap.Error(err))
			continue
		}
		pool.Release(acc, "")
		httpStatus = http.StatusOK
		c.JSON(httpStatus, result)
		globalLog.Add(LogEntry{
			Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/chat/completions",
			Model: body.Model, Account: usedEmail, Status: httpStatus,
			Duration: time.Since(start).Milliseconds(), Stream: false,
			TokensIn: approxTokens(body.Messages), TokensOut: resultTokenCount(result),
			RemoteIP: c.ClientIP(),
		})
		return
	}
	httpStatus = http.StatusServiceUnavailable
	c.JSON(httpStatus, gin.H{"error": fmt.Sprintf("All retries failed: %s", lastErr)})
	globalLog.Add(LogEntry{
		Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/chat/completions",
		Model: body.Model, Account: usedEmail, Status: httpStatus,
		Duration: time.Since(start).Milliseconds(), Stream: body.Stream,
		Error: lastErr, RemoteIP: c.ClientIP(),
	})
}

func nonStreamOpenAI(
	ctx context.Context,
	engine *engineClient,
	pool *AccountPool,
	acc *Account,
	messages []map[string]interface{},
	adapter ChatAdapter,
	modelName string,
	tools []map[string]interface{},
) (map[string]interface{}, error) {
	ch, err := SendChatAndStreamWithTools(ctx, engine, acc, messages, adapter, nil, tools)
	if err != nil {
		return nil, err
	}
	var full string
	for r := range ch {
		if r.Err != nil {
			return nil, r.Err
		}
		if r.Done {
			break
		}
		full += r.Delta
	}
	pool.IncrementSuccess(acc)
	// usage：用 rune 数做粗略 token 估算
	// 中文 1 rune ≈ 1 token；英文 4 chars ≈ 1 token。取 rune 数 ÷ 2 做折中。
	promptTok := approxTokens(messages)
	complTok := utf8.RuneCountInString(full) / 2
	if complTok < 1 && full != "" {
		complTok = 1
	}

	// 解析工具调用
	toolCalls, cleaned, hasCalls := ParseToolCalls(full)

	// 构建响应 message
	msg := map[string]interface{}{
		"role":    "assistant",
		"content": cleaned,
	}
	finishReason := "stop"
	if hasCalls {
		msg["tool_calls"] = toolCalls
		finishReason = "tool_calls"
		// 如果清理后内容为空，设为 null（OpenAI 规范）
		if cleaned == "" {
			msg["content"] = nil
		}
	}

	return map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%s", shortUUID()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   firstNonEmpty(modelName, adapter.DisplayLabel, "cto-proxy"),
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msg,
				"finish_reason": finishReason,
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     promptTok,
			"completion_tokens": complTok,
			"total_tokens":      promptTok + complTok,
		},
	}, nil
}

// resultTokenCount 从 OpenAI 响应中提取 completion_tokens
func resultTokenCount(result map[string]interface{}) int {
	if usage, ok := result["usage"].(map[string]int); ok {
		return usage["completion_tokens"]
	}
	return 0
}

// streamOpenAI 流式回传 cto.new 的生成结果，按 OpenAI 规范分块输出。
//
// 工具调用的处理策略：
//   cto.new 的上游模型以纯文本形式输出 ```tool_call {...}``` 块，
//   opencode/OpenAI SDK 期望的是结构化的 tool_calls 字段。
//   所以必须等整段响应收完再解析，否则无法区分 "这是工具调用" 还是 "这是普通文本"。
//   折中方案：
//     1) 先发 role chunk 让客户端知道响应开始；
//     2) 边收 delta 边缓冲，同时每积累一小段就转发 content chunk（改善用户感知）；
//     3) 全部收完后，若检测到 tool_call 块：
//        - 回滚已发的 content（发一段空 delta 覆盖），再一次性发 tool_calls chunk；
//        - 但大多数 SDK 不支持回滚。实际做法是：检测到 tool_call 时缓冲模式，不边发边转发。
//   实现上：如果客户端传入了 tools 参数，则进入全缓冲模式；否则流式转发。
func streamOpenAI(
	c *gin.Context, pool *AccountPool, acc *Account,
	ch <-chan WSResult, modelName string, start time.Time, remoteIP string,
) {
	cid := fmt.Sprintf("chatcmpl-%s", shortUUID())
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Connection", "keep-alive")

	// 起始 chunk 让 client 知道 role
	writeSSE(c, map[string]interface{}{
		"id":      cid,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   firstNonEmpty(modelName, "cto-proxy"),
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]string{"role": "assistant"}, "finish_reason": nil},
		},
	})

	// 全缓冲收集整段响应
	var buf strings.Builder
	var streamErr string
	ctx := c.Request.Context()
	success := true
Collect:
	for {
		select {
		case <-ctx.Done():
			// 客户端断开
			streamErr = "context canceled"
			success = false
			break Collect
		case r, ok := <-ch:
			if !ok {
				break Collect
			}
			if r.Err != nil {
				streamErr = r.Err.Error()
				success = false
				pool.Release(acc, r.Err.Error())
				break Collect
			}
			if r.Done {
				break Collect
			}
			buf.WriteString(r.Delta)
		}
	}

	fullText := buf.String()

	// 解析工具调用
	toolCalls, cleaned, hasCalls := ParseToolCalls(fullText)

	// 构造并发送响应 chunks
	finishReason := "stop"
	if success {
		if hasCalls {
			// 先发带内容的 chunk（cleaned 可能为空）
			if cleaned != "" {
				writeSSE(c, map[string]interface{}{
					"id":      cid,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   firstNonEmpty(modelName, "cto-proxy"),
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]string{"content": cleaned}, "finish_reason": nil},
					},
				})
			}
			// 再发 tool_calls chunks（OpenAI 流式规范：每个 tool_call 两段：name 和 arguments）
			for i, tc := range toolCalls {
				fn, _ := tc["function"].(map[string]interface{})
				tcName, _ := fn["name"].(string)
				tcArgs, _ := fn["arguments"].(string)
				tcID, _ := tc["id"].(string)
				// 第一段：id + type + name + arguments（大多数 SDK 接受一次性完整 chunk）
				writeSSE(c, map[string]interface{}{
					"id":      cid,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   firstNonEmpty(modelName, "cto-proxy"),
					"choices": []map[string]interface{}{
						{"index": 0, "delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{
								{
									"index": i, "id": tcID, "type": "function",
									"function": map[string]interface{}{
										"name":      tcName,
										"arguments": tcArgs,
									},
								},
							},
						}, "finish_reason": nil},
					},
				})
			}
			finishReason = "tool_calls"
		} else if cleaned != "" {
			// 普通文本响应：一次性作为 content chunk 发出
			writeSSE(c, map[string]interface{}{
				"id":      cid,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   firstNonEmpty(modelName, "cto-proxy"),
				"choices": []map[string]interface{}{
					{"index": 0, "delta": map[string]string{"content": cleaned}, "finish_reason": nil},
				},
			})
		}
	}

	// finish chunk
	writeSSE(c, map[string]interface{}{
		"id":      cid,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   firstNonEmpty(modelName, "cto-proxy"),
		"choices": []map[string]interface{}{
			{"index": 0, "delta": map[string]interface{}{}, "finish_reason": finishReason},
		},
	})
	fmt.Fprint(c.Writer, "data: [DONE]\n\n")
	c.Writer.Flush()

	if success {
		pool.IncrementSuccess(acc)
		pool.Release(acc, "")
	} else {
		pool.Release(acc, streamErr)
	}

	logStatus := 200
	if !success {
		logStatus = 502
	}
	globalLog.Add(LogEntry{
		Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/chat/completions",
		Model: modelName, Account: acc.Email, Status: logStatus,
		Duration: time.Since(start).Milliseconds(), Stream: true,
		Error: streamErr, RemoteIP: remoteIP,
	})
}

// writeSSE 一次性写（flush 由 c.Writer 管理）
func writeSSE(c *gin.Context, data map[string]interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(c.Writer, "data: %s\n\n", b)
	c.Writer.Flush()
}

func writeSSEWriter(w io.Writer, data map[string]interface{}) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "data: %s\n\n", b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

// approxTokens 非常粗略的 token 数。用 rune 计数而不是 byte 长度：
//   - 中文：rune ≈ token，len(bytes)/4 严重低估
//   - 英文：4 chars ≈ 1 token，rune/2 略高估但不影响 SDK
// OpenAI SDK 只把 usage 当展示值，数量级合理即可。
func approxTokens(messages []map[string]interface{}) int {
	total := 0
	for _, m := range messages {
		total += utf8.RuneCountInString(extractContent(m["content"]))
	}
	return total / 2
}
