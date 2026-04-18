package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// handleAnthropicMessages 实现 Anthropic 兼容的 POST /v1/messages
// 请求体格式参考 https://docs.anthropic.com/en/api/messages
func handleAnthropicMessages(c *gin.Context, pool *AccountPool, engine *engineClient) {
	start := time.Now()
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	model, _ := body["model"].(string)
	stream, _ := body["stream"].(bool)
	rawMsgs, _ := body["messages"].([]interface{})
	system := body["system"]

	messages := convertAnthropicMessages(rawMsgs)
	if sys := extractSystemText(system); sys != "" {
		messages = append(
			[]map[string]interface{}{{"role": "system", "content": sys}},
			messages...,
		)
	}
	if len(messages) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "messages is required"})
		return
	}
	adapter := ResolveModel(model)
	log.Info("anthropic: new request",
		zap.String("model", model),
		zap.String("engineKey", adapter.EngineKey),
		zap.Bool("stream", stream),
		zap.Int("msgs", len(messages)))

	var lastErr string
	var usedEmail string
	var httpStatus int
	for retry := 0; retry < MaxRetries; retry++ {
		acc := pool.Get()
		if acc == nil {
			httpStatus = http.StatusServiceUnavailable
			c.JSON(httpStatus, gin.H{"error": "No available accounts"})
			globalLog.Add(LogEntry{
				Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/messages",
				Model: model, Account: "", Status: httpStatus,
				Duration: time.Since(start).Milliseconds(), Stream: stream,
				Error: "no_available_accounts", RemoteIP: c.ClientIP(),
			})
			return
		}
		usedEmail = acc.Email

		if stream {
			ch, err := SendChatAndStream(c.Request.Context(), engine, acc,
				messages, adapter, nil)
			if err != nil {
				lastErr = err.Error()
				// 413 消息过大，重试无意义，直接返回错误
				if strings.Contains(lastErr, "413") || strings.Contains(lastErr, "Message too large") {
					pool.Release(acc, "")
					c.JSON(http.StatusRequestEntityTooLarge, gin.H{
						"type":  "error",
						"error": map[string]interface{}{
							"type":    "invalid_request_error",
							"message": "Prompt too large for upstream API (413). Reduce message count or length.",
						},
					})
					globalLog.Add(LogEntry{
						Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/messages",
						Model: model, Account: usedEmail, Status: 413,
						Duration: time.Since(start).Milliseconds(), Stream: stream,
						Error: "message_too_large", RemoteIP: c.ClientIP(),
					})
					return
				}
				pool.Release(acc, err.Error())
				continue
			}
			streamAnthropic(c, pool, acc, ch, model, start, c.ClientIP())
			return
		}

		result, err := nonStreamAnthropic(c.Request.Context(), engine, pool, acc,
			messages, adapter, model)
		if err != nil {
			lastErr = err.Error()
			// 413 消息过大，直接返回
			if strings.Contains(lastErr, "413") || strings.Contains(lastErr, "Message too large") {
				pool.Release(acc, "")
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{
					"type":  "error",
					"error": map[string]interface{}{
						"type":    "invalid_request_error",
						"message": "Prompt too large for upstream API (413). Reduce message count or length.",
					},
				})
				globalLog.Add(LogEntry{
					Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/messages",
					Model: model, Account: usedEmail, Status: 413,
					Duration: time.Since(start).Milliseconds(), Stream: false,
					Error: "message_too_large", RemoteIP: c.ClientIP(),
				})
				return
			}
			pool.Release(acc, err.Error())
			continue
		}
		pool.Release(acc, "")
		httpStatus = http.StatusOK
		c.JSON(httpStatus, result)
		globalLog.Add(LogEntry{
			Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/messages",
			Model: model, Account: usedEmail, Status: httpStatus,
			Duration: time.Since(start).Milliseconds(), Stream: false,
			TokensIn: approxTokens(messages), RemoteIP: c.ClientIP(),
		})
		return
	}
	httpStatus = http.StatusServiceUnavailable
	c.JSON(httpStatus, gin.H{"error": fmt.Sprintf("All retries failed: %s", lastErr)})
	globalLog.Add(LogEntry{
		Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/messages",
		Model: model, Account: usedEmail, Status: httpStatus,
		Duration: time.Since(start).Milliseconds(), Stream: stream,
		Error: lastErr, RemoteIP: c.ClientIP(),
	})
}

func nonStreamAnthropic(
	ctx context.Context,
	engine *engineClient,
	pool *AccountPool,
	acc *Account,
	messages []map[string]interface{},
	adapter ChatAdapter,
	modelName string,
) (map[string]interface{}, error) {
	ch, err := SendChatAndStream(ctx, engine, acc, messages, adapter, nil)
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
	outTok := utf8.RuneCountInString(full) / 2
	if outTok < 1 && full != "" {
		outTok = 1
	}
	return map[string]interface{}{
		"id":   fmt.Sprintf("msg_%s", shortUUID()),
		"type": "message",
		"role": "assistant",
		"content": []map[string]interface{}{
			{"type": "text", "text": full},
		},
		"model":         firstNonEmpty(modelName, adapter.DisplayLabel, "cto-proxy"),
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]int{
			"input_tokens":  approxTokens(messages),
			"output_tokens": outTok,
		},
	}, nil
}

func streamAnthropic(
	c *gin.Context, pool *AccountPool, acc *Account,
	ch <-chan WSResult, modelName string, start time.Time, remoteIP string,
) {
	mid := fmt.Sprintf("msg_%s", shortUUID())
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("Connection", "keep-alive")

	// message_start
	msgStart, _ := json.Marshal(map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            mid,
			"type":          "message",
			"role":          "assistant",
			"model":         firstNonEmpty(modelName, "cto-proxy"),
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	writeRawSSE(c, "message_start", msgStart)
	c.Writer.Flush()

	// content_block_start
	blockStart, _ := json.Marshal(map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	writeRawSSE(c, "content_block_start", blockStart)
	c.Writer.Flush()

	// 收尾函数：无论正常/异常都发完 Anthropic 规范里的完整事件序列，防止客户端挂死
	outRunes := 0
	ended := false
	var streamErr string
	finish := func(stopReason, stopSequenceErr string, success bool) {
		if ended {
			return
		}
		ended = true
		if stopSequenceErr != "" {
			streamErr = stopSequenceErr
		}

		// content_block_stop
		blockStop, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_stop",
			"index": 0,
		})
		writeRawSSE(c, "content_block_stop", blockStop)
		c.Writer.Flush()

		// message_delta
		msgDelta, _ := json.Marshal(map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]int{"output_tokens": outRunes / 2},
		})
		writeRawSSE(c, "message_delta", msgDelta)
		c.Writer.Flush()

		// message_stop（如果有错误，顺带发一条 error 事件便于客户端定位）
		if stopSequenceErr != "" {
			errEvt, _ := json.Marshal(map[string]interface{}{
				"type": "error",
				"error": map[string]string{
					"type":    "upstream_error",
					"message": stopSequenceErr,
				},
			})
			writeRawSSE(c, "error", errEvt)
			c.Writer.Flush()
		}
		msgStop, _ := json.Marshal(map[string]string{"type": "message_stop"})
		writeRawSSE(c, "message_stop", msgStop)
		c.Writer.Flush()

		if success {
			pool.IncrementSuccess(acc)
			pool.Release(acc, "")
		}
		// 记录流式请求日志
		logStatus := 200
		if !success {
			logStatus = 502
		}
		globalLog.Add(LogEntry{
			Time: time.Now().Format(time.RFC3339), Method: "POST", Path: "/v1/messages",
			Model: modelName, Account: acc.Email, Status: logStatus,
			Duration: time.Since(start).Milliseconds(), Stream: true,
			Error: streamErr, RemoteIP: remoteIP,
		})
	}

	for r := range ch {
		if r.Err != nil {
			finish("end_turn", r.Err.Error(), false)
			pool.Release(acc, r.Err.Error())
			return
		}
		if r.Done {
			break
		}
		if r.Delta == "" {
			continue
		}
		outRunes += utf8.RuneCountInString(r.Delta)
		delta, _ := json.Marshal(map[string]interface{}{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]string{
				"type": "text_delta",
				"text": r.Delta,
			},
		})
		writeRawSSE(c, "content_block_delta", delta)
		c.Writer.Flush()
	}
	finish("end_turn", "", true)
}

func writeRawSSE(c *gin.Context, event string, data []byte) {
	fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data)
}

// convertAnthropicMessages 把 Anthropic 的 messages 转成内部统一格式
// Anthropic: content 可以是 string 或 []{type:text|image, ...}
func convertAnthropicMessages(raw []interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, m := range raw {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, msg)
	}
	return out
}

// extractSystemText system 可能是 string 或 content block 数组
func extractSystemText(system interface{}) string {
	switch s := system.(type) {
	case string:
		return s
	case []interface{}:
		var parts []string
		for _, item := range s {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					parts = append(parts, t)
				}
			} else if str, ok := item.(string); ok {
				parts = append(parts, str)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
