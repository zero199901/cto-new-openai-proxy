package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// WSResult 每个 WebSocket 帧经过解析后产生的增量结果
type WSResult struct {
	Delta string // 这次的文本增量
	Done  bool   // 服务端已标记结束
	Err   error  // 致命错误（会关闭 chan）
}

// connectAndStream 连接 cto.new 的流式 WS，把增量持续写进返回的 channel。
// 流程：
//   1. POST /engine-agent/offers              (HTTP)
//   2. POST /engine-agent/chat  (携带 prompt) (HTTP, 202)
//   3. 收 WS 帧，提取 {"type":"update","buffer":"{...chat...}"}
//   4. 遇到 state.inProgress=false 时关闭 chan
func connectAndStream(
	ctx context.Context,
	engine *engineClient,
	acc *Account,
	chatHistoryID, workspaceID, prompt string,
	imageURLs []string,
) (<-chan WSResult, error) {
	ch := make(chan WSResult, 64)

	// 先启 WS，再发 offers+chat。顺序颠倒会丢前几个 token。
	wsCtx, cancel := context.WithTimeout(ctx, time.Duration(WSTimeout)*time.Second)
	ready := make(chan struct{})
	go func() {
		defer cancel()
		wsConsume(wsCtx, acc, chatHistoryID, workspaceID, ch, ready)
	}()

	// 等 WS 握手完成
	select {
	case <-ready:
	case <-time.After(8 * time.Second):
		cancel()
		return nil, fmt.Errorf("ws connect timeout")
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}

	// offers
	if err := engine.sendOffers(acc, chatHistoryID); err != nil {
		cancel()
		return nil, fmt.Errorf("offers: %w", err)
	}
	// chat 正文
	if err := engine.sendPrompt(acc, chatHistoryID, prompt, imageURLs); err != nil {
		cancel()
		return nil, fmt.Errorf("send prompt: %w", err)
	}
	return ch, nil
}

// wsConsume 负责 WS 生命周期：连接 → 读帧 → 解析 → 转发 → 关闭
func wsConsume(
	ctx context.Context,
	acc *Account,
	chatHistoryID, workspaceID string,
	ch chan<- WSResult,
	ready chan struct{},
) {
	defer close(ch)

	tokenSuffix := acc.WSTokenSuffix
	if tokenSuffix == "" {
		tokenSuffix = acc.ClerkUserID + ":" + acc.ClerkOrgID
	}
	wsURL := fmt.Sprintf(
		"%s/engine-agent/chat-histories/%s/buffer/stream?token=%s&workspaceId=%s",
		EngineWS, chatHistoryID,
		url.QueryEscape(tokenSuffix),
		url.QueryEscape(workspaceID),
	)

	headers := http.Header{}
	headers.Set("Origin", "https://cto.new")
	headers.Set("Cookie", buildClerkCookie(acc))
	headers.Set("User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")

	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		ch <- WSResult{Err: fmt.Errorf("ws dial: %w (status=%v)", err, httpStatus(resp))}
		safeClose(ready)
		return
	}
	defer conn.Close()
	safeClose(ready)

	// 后台心跳：每 30s 发 "ping"
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = conn.WriteMessage(websocket.TextMessage, []byte("ping"))
			}
		}
	}()

	log.Info("ws: connected",
		zap.String("email", acc.Email),
		zap.String("chat", truncate(chatHistoryID, 16)))

	// 记录当前正在累积的 assistant 消息 id（同一个 msg id 的 chat.content 拼接在一起）
	currentMsgID := ""
	sentLen      := 0    // 已发送的文本长度（WS buffer 是累积全文，需提取增量）
	firstUpdate  := true // 首帧调试标记
	wasActive    := false // 是否曾收到 inProgress=true（区分初始 idle 和生成结束）

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn.SetReadDeadline(time.Now().Add(time.Duration(WSTimeout) * time.Second))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("ws: read error", zap.Error(err), zap.String("email", acc.Email))
				ch <- WSResult{Err: fmt.Errorf("ws read: %w", err)}
			}
			return
		}

		// 心跳响应："pong"
		if string(raw) == "pong" {
			continue
		}

		var top struct {
			Type   string          `json:"type"`
			Buffer json.RawMessage `json:"buffer"`
			Label  string          `json:"label"`
			State  struct {
				InProgress bool `json:"inProgress"`
				Cancelled  bool `json:"cancelled"`
			} `json:"state"`
		}
		if err := json.Unmarshal(raw, &top); err != nil {
			log.Debug("ws: unmarshal top-level failed",
				zap.Error(err),
				zap.String("raw", truncate(string(raw), 200)))
			continue
		}

		log.Debug("ws: frame",
			zap.String("type", top.Type),
			zap.Bool("inProgress", top.State.InProgress),
			zap.String("buffer", truncate(string(top.Buffer), 200)),
			zap.String("label", top.Label))

		switch top.Type {
		case "state":
			if top.State.InProgress {
				wasActive = true
				continue
			}
			// inProgress=false：
			//   - wasActive=true  → 生成结束，正常结束
			//   - wasActive=false → 初始 idle 状态（WS 连接后立即推送），忽略继续等
			if !wasActive {
				log.Debug("ws: initial idle state, ignoring",
					zap.String("email", acc.Email))
				continue
			}
			if top.State.Cancelled {
				log.Warn("ws: upstream cancelled",
					zap.String("email", acc.Email))
			}
			ch <- WSResult{Done: true}
			return

		case "update":
			// buffer 既可能是 JSON 字符串也可能是对象；两种都处理
			var bufStr string
			if err := json.Unmarshal(top.Buffer, &bufStr); err != nil {
				bufStr = string(top.Buffer)
			}
			if bufStr == "" {
				continue
			}
			// 首帧调试：打印 buffer 结构帮助排查
			if firstUpdate {
				firstUpdate = false
				log.Info("ws: first update frame",
					zap.String("email", acc.Email),
					zap.String("buffer", truncate(bufStr, 500)))
			}
			// 使用通用 map 解析，兼容多种消息格式
			var p map[string]interface{}
			if err := json.Unmarshal([]byte(bufStr), &p); err != nil {
				log.Debug("ws: unmarshal buffer failed",
					zap.Error(err), zap.String("raw", truncate(bufStr, 200)))
				continue
			}
			role, _ := p["role"].(string)
			if role == "user" {
				continue
			}
			msgID, _ := p["id"].(string)
			// 新消息 ID → 重置累积长度
			if msgID != "" && msgID != currentMsgID {
				currentMsgID = msgID
				sentLen = 0
			}
			// ---- 提取文本内容 ----
			// WS 帧有两种格式，按提取来源区分：
			//   A) chat.content → 增量格式：每帧只含新 token（cto.new 当前行为）
			//   B) 顶层 content 字符串 → 可能是累积格式：每帧含完整当前文本
			//   C) content 数组 → 用户消息回显，直接发送
			// 关键：不能靠 len(textChunk) vs sentLen 判断，因为增量帧 " there"(len=6)
			// 可能 > sentLen(2)，被误判为累积格式做差导致内容丢失
			var textChunk string
			isIncremental := true // 默认增量，只有方式2（顶层 content）才可能是累积
			// 方式1: chat.content（字符串，增量格式）
			if chatObj, ok := p["chat"].(map[string]interface{}); ok {
				if c, ok := chatObj["content"].(string); ok && c != "" {
					textChunk = c
				}
			}
			// 方式2: 顶层 content 为字符串（可能是累积格式）
			if textChunk == "" {
				if c, ok := p["content"].(string); ok && c != "" {
					textChunk = c
					isIncremental = false
				}
			}
			// 方式3: content 数组（content blocks 格式，增量）
			if textChunk == "" {
				if arr, ok := p["content"].([]interface{}); ok {
					for _, item := range arr {
						if block, ok := item.(map[string]interface{}); ok {
							t, _ := block["type"].(string)
							if t == "text" || t == "" {
								if txt, ok := block["text"].(string); ok && txt != "" {
									textChunk += txt
								}
							}
						}
					}
				}
			}
			if textChunk == "" {
				continue
			}
			// 增量格式直接发送；累积格式做差提取新增部分
			var delta string
			if isIncremental {
				delta = textChunk
			} else {
				// 累积格式：取尾部新增部分
				if len(textChunk) > sentLen {
					delta = textChunk[sentLen:]
				}
				sentLen = len(textChunk)
			}
			if delta != "" {
				log.Debug("ws: sending delta",
					zap.Int("deltaLen", len(delta)),
					zap.Int("sentLen", sentLen),
					zap.String("sample", truncate(delta, 60)))
				select {
				case ch <- WSResult{Delta: delta}:
				case <-ctx.Done():
					return
				}
			}

		case "label":
			// 对话自动命名，忽略

		case "history":
			// 连接一开始会推 history，无需再转发
		}
	}
}

func safeClose(c chan struct{}) {
	defer func() { _ = recover() }()
	select {
	case <-c:
	default:
		close(c)
	}
}

func httpStatus(r *http.Response) int {
	if r == nil {
		return 0
	}
	return r.StatusCode
}
