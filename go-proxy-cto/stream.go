package main

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"
)

// SendChatAndStream 对外的封装入口：发消息 + 接流
//   1) 切模型（PATCH /teams/preferred-model）
//   2) 创建临时 project（/projects/create-hosted），拿到 chatHistoryId/workspaceId
//   3) 连 WS + 发 offers + 发 chat
// 返回 <-chan WSResult，上层读到 Done/Err 为止。
func SendChatAndStream(
	ctx context.Context,
	engine *engineClient,
	acc *Account,
	messages []map[string]interface{},
	adapter ChatAdapter,
	imageURLs []string,
) (<-chan WSResult, error) {
	return SendChatAndStreamWithTools(ctx, engine, acc, messages, adapter, imageURLs, nil)
}

// SendChatAndStreamWithTools 支持工具调用的版本
func SendChatAndStreamWithTools(
	ctx context.Context,
	engine *engineClient,
	acc *Account,
	messages []map[string]interface{},
	adapter ChatAdapter,
	imageURLs []string,
	tools []map[string]interface{},
) (<-chan WSResult, error) {
	// 1) 切模型（简单起见每次都切；API 幂等）
	if err := engine.setPreferredModel(acc, adapter.EngineKey); err != nil {
		log.Warn("stream: set model failed",
			zap.String("email", acc.Email),
			zap.String("model", adapter.EngineKey),
			zap.Error(err))
		// 非致命，继续
	}

	// 2) 拼 prompt（含工具描述）
	prompt := FormatMessages(messages, tools)
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("empty prompt")
	}

	// 3) 新建 hosted project
	proj, err := engine.createHostedProject(acc, genProjectName())
	if err != nil {
		return nil, err
	}

	// 4) 连 WS + 发消息
	return connectAndStream(ctx, engine, acc,
		proj.ChatHistoryID, proj.WorkspaceID, prompt, imageURLs)
}

// MaxPromptBytes cto.new Engine API 对 prompt 有大小限制（实测约 32KB body），
// body 内容是 base64 后的 prompt，base64 膨胀 1.33 倍。
// 设 20000 字节作为安全上限（base64 后约 27KB，body 加上其他 JSON 字段约 28KB）。
// 注意：用字节数而非字符数，因为中文 1 字符=3 字节，按字符限制会严重超上游限制。
const MaxPromptBytes = 20000

// FormatMessages 将 OpenAI / Anthropic 格式的多轮 messages 拼成一个 prompt
// cto.new 的 /engine-agent/chat 只接受字符串，不支持 messages[] 数组。
// tools 参数可选：如果提供，会在 system prompt 后注入工具描述。
// 如果 prompt 超过 MaxPromptBytes（字节数），会截断早期对话历史，保留 system + 最近对话。
func FormatMessages(messages []map[string]interface{}, tools []map[string]interface{}) string {
	var parts []string

	// 如果有工具定义，在最前面注入工具描述
	if len(tools) > 0 {
		toolDesc := formatToolsDescription(tools)
		if toolDesc != "" {
			parts = append(parts, toolDesc)
		}
	}

	for _, m := range messages {
		role, _ := m["role"].(string)
		content := extractContent(m["content"])
		if strings.TrimSpace(content) == "" {
			continue
		}
		switch role {
		case "system":
			parts = append(parts, fmt.Sprintf("[System]\n%s\n[/System]", content))
		case "assistant":
			parts = append(parts, fmt.Sprintf("Assistant: %s", content))
		case "user", "":
			parts = append(parts, content)
		default:
			parts = append(parts, fmt.Sprintf("%s: %s", role, content))
		}
	}

	prompt := strings.Join(parts, "\n\n")

	// 按字节数（UTF-8）判断是否需要截断
	if len(prompt) > MaxPromptBytes {
		prompt = truncatePrompt(messages, tools, MaxPromptBytes)
	}
	// 最终兜底：如果截断逻辑仍未压到限制内，强制硬截断
	if len(prompt) > MaxPromptBytes {
		prompt = safeTruncateBytes(prompt, MaxPromptBytes)
	}
	return prompt
}

// truncatePrompt 截断过长的 prompt，优先级：
// 1. 保留 system + 最近对话
// 2. 如果 system 本身就超限，截断 system
// maxBytes 为 UTF-8 字节上限。
func truncatePrompt(messages []map[string]interface{}, tools []map[string]interface{}, maxBytes int) string {
	var systemParts []string
	var dialogParts []string

	if len(tools) > 0 {
		toolDesc := formatToolsDescription(tools)
		if toolDesc != "" {
			systemParts = append(systemParts, toolDesc)
		}
	}

	for _, m := range messages {
		role, _ := m["role"].(string)
		content := extractContent(m["content"])
		if strings.TrimSpace(content) == "" {
			continue
		}
		if role == "system" {
			systemParts = append(systemParts, fmt.Sprintf("[System]\n%s\n[/System]", content))
			continue
		}
		switch role {
		case "assistant":
			dialogParts = append(dialogParts, fmt.Sprintf("Assistant: %s", content))
		case "user", "":
			dialogParts = append(dialogParts, content)
		default:
			dialogParts = append(dialogParts, fmt.Sprintf("%s: %s", role, content))
		}
	}

	systemText := strings.Join(systemParts, "\n\n")
	systemBytes := len(systemText)

	// 预留给对话的空间（占总量 40%，不少于 2000 字节）
	dialogBudget := maxBytes * 4 / 10
	if dialogBudget < 2000 {
		dialogBudget = 2000
	}
	systemBudget := maxBytes - dialogBudget - 100 // 100 字节余量用于分隔符

	// 如果 system 超过配额，截断 system
	if systemBytes > systemBudget {
		systemText = safeTruncateBytes(systemText, systemBudget) + "\n...[system truncated]"
		systemBytes = len(systemText)
	}

	remaining := maxBytes - systemBytes - 50

	// 从后往前收集对话
	var kept []string
	usedBytes := 0
	for i := len(dialogParts) - 1; i >= 0; i-- {
		partBytes := len(dialogParts[i]) + 2 // "\n\n"
		if usedBytes+partBytes > remaining {
			// 如果第一轮就塞不下，截断这一轮并停止
			if len(kept) == 0 && remaining > 200 {
				truncated := safeTruncateBytes(dialogParts[i], remaining-50) + "\n...[truncated]"
				kept = append(kept, truncated)
			}
			break
		}
		kept = append([]string{dialogParts[i]}, kept...)
		usedBytes += partBytes
	}

	if len(kept) == 0 {
		return systemText
	}
	return systemText + "\n\n" + strings.Join(kept, "\n\n")
}

// safeTruncateBytes 按字节数截断字符串，确保不切断 UTF-8 字符
func safeTruncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// 从 maxBytes 往前回退，直到找到完整的 UTF-8 字符边界
	b := []byte(s)
	for i := maxBytes; i > 0; i-- {
		if utf8.RuneStart(b[i]) {
			return string(b[:i])
		}
	}
	return ""
}

// unwrapTool 兼容两种 tools 格式：
//   - OpenAI 嵌套：{type: "function", function: {name, description, parameters}}
//   - 扁平：{name, description, parameters}
// 返回统一的扁平 map；无法识别时返回 nil。
func unwrapTool(t map[string]interface{}) map[string]interface{} {
	// 扁平格式
	if _, hasName := t["name"].(string); hasName {
		return t
	}
	// OpenAI 嵌套格式
	if fn, ok := t["function"].(map[string]interface{}); ok {
		return fn
	}
	return nil
}

// formatToolsDescription 将 OpenAI tools 格式转为自然语言描述注入 prompt
// 兼容 OpenAI 嵌套 {type, function:{...}} 和扁平 {name, ...} 两种格式。
//
// 重要：cto.new 的上游模型（gpt-5.4 / glm-5.1）**没有**被训练成原生工具调用模型，
// 所以必须用极其直白、强制的 prompt 引导模型输出 `tool_call` 块。
// prompt 设计要点：
//   1. 明确告知模型"你没有直接访问文件/网络/命令的能力"
//   2. 给出正反例（few-shot），让模型学会"遇到此类请求 → 先输出 tool_call"
//   3. 反复强调代码围栏必须是 ```tool_call
func formatToolsDescription(tools []map[string]interface{}) string {
	var lines []string
	lines = append(lines,
		"# TOOL USE PROTOCOL (MANDATORY)")
	lines = append(lines, "")
	lines = append(lines,
		"You are an AI assistant running through a proxy. You do NOT have any built-in ability to:")
	lines = append(lines, "- Read files or directories on the user's system")
	lines = append(lines, "- Execute shell commands")
	lines = append(lines, "- Browse the web")
	lines = append(lines, "- Access any external state")
	lines = append(lines, "")
	lines = append(lines,
		"The ONLY way for you to get this information is to CALL A TOOL from the list below. "+
			"The client (not you) will execute the tool and send the result back to you in a "+
			"follow-up message with role=\"tool\".")
	lines = append(lines, "")
	lines = append(lines,
		"If you invent or guess file contents, directory listings, or command output instead of "+
			"calling a tool, you have FAILED the task.")
	lines = append(lines, "")
	lines = append(lines, "## Output format")
	lines = append(lines, "")
	lines = append(lines,
		"To call a tool, output a fenced code block with the EXACT language `tool_call` "+
			"(not ```json, not ```):")
	lines = append(lines, "```tool_call")
	lines = append(lines, `{"name": "<tool_name>", "arguments": {"<key>": "<value>"}}`)
	lines = append(lines, "```")
	lines = append(lines, "")
	lines = append(lines,
		"The fence must contain ONLY a JSON object — no prose, no extra keys. "+
			"You may emit multiple `tool_call` blocks back-to-back if you need to call several tools.")
	lines = append(lines, "")
	lines = append(lines, "## Examples")
	lines = append(lines, "")
	lines = append(lines, "User: list the files in /tmp")
	lines = append(lines, "Assistant:")
	lines = append(lines, "```tool_call")
	lines = append(lines, `{"name": "list_directory", "arguments": {"path": "/tmp"}}`)
	lines = append(lines, "```")
	lines = append(lines, "")
	lines = append(lines, "User: what does package.json say")
	lines = append(lines, "Assistant:")
	lines = append(lines, "```tool_call")
	lines = append(lines, `{"name": "read_file", "arguments": {"path": "package.json"}}`)
	lines = append(lines, "```")
	lines = append(lines, "")
	lines = append(lines, "## Available tools")
	lines = append(lines, "")
	for _, raw := range tools {
		t := unwrapTool(raw)
		if t == nil {
			continue
		}
		name, _ := t["name"].(string)
		desc, _ := t["description"].(string)
		if name == "" {
			continue
		}
		entry := fmt.Sprintf("- **%s**: %s", name, desc)
		if params, ok := t["parameters"].(map[string]interface{}); ok {
			if props, ok := params["properties"].(map[string]interface{}); ok && len(props) > 0 {
				req, _ := params["required"].([]interface{})
				entry += " Parameters:"
				for k, v := range props {
					if p, ok := v.(map[string]interface{}); ok {
						pdesc, _ := p["description"].(string)
						ptype, _ := p["type"].(string)
						reqFlag := ""
						for _, r := range req {
							if rs, ok := r.(string); ok && rs == k {
								reqFlag = " (required)"
								break
							}
						}
						entry += fmt.Sprintf(" `%s`(%s%s): %s;", k, ptype, reqFlag, pdesc)
					}
				}
			}
		}
		lines = append(lines, entry)
	}
	lines = append(lines, "")
	lines = append(lines, "## Rules")
	lines = append(lines, "")
	lines = append(lines,
		"1. If the user asks you to inspect, read, list, search, run, fetch, or otherwise interact "+
			"with any real-world resource, your FIRST response MUST be one or more `tool_call` blocks.")
	lines = append(lines,
		"2. Do not apologize, explain, or describe what you would do — just emit the tool_call.")
	lines = append(lines,
		"3. Only reply in normal prose once you actually have the information needed to answer, "+
			"i.e. after the client has sent back a role=\"tool\" message with the result.")
	lines = append(lines,
		"4. Never fabricate tool results. Never guess. If unsure, call a tool.")
	return strings.Join(lines, "\n")
}

// extractContent 兼容多种内容结构：
//   - string                            → 直接返回
//   - [{type:text, text:...}]           → 拼接 text
//   - [{type:image_url, image_url:{url}}]→ 以 "[image: <url>]" 占位
//   - [{type:image, source:{...}}]      → Anthropic 图像块，以 "[image]" 占位
// 目的是：即便 cto.new 不支持图像，也不让消息内容神秘消失（避免客户端看到空响应）。
func extractContent(v interface{}) string {
	switch c := v.(type) {
	case string:
		return c
	case []interface{}:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]interface{}); ok {
				t, _ := m["type"].(string)
				switch t {
				case "text":
					if text, _ := m["text"].(string); text != "" {
						parts = append(parts, text)
					}
				case "image_url":
					// OpenAI 多模态格式
					if img, ok := m["image_url"].(map[string]interface{}); ok {
						if u, _ := img["url"].(string); u != "" {
							parts = append(parts, fmt.Sprintf("[image: %s]", truncate(u, 120)))
						}
					} else if u, _ := m["image_url"].(string); u != "" {
						parts = append(parts, fmt.Sprintf("[image: %s]", truncate(u, 120)))
					}
				case "image":
					// Anthropic 格式
					parts = append(parts, "[image]")
				case "input_text":
					// Anthropic 的另一种 text 变体
					if text, _ := m["text"].(string); text != "" {
						parts = append(parts, text)
					}
				}
			} else if s, ok := item.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}
