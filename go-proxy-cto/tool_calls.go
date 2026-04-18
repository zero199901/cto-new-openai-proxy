package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// toolCallRegex 匹配模型输出中的 ```tool_call ... ``` 块
var toolCallRegex = regexp.MustCompile("(?s)```tool_call\\s*\\n(.*?)\\n```")

// ParseToolCalls 从模型输出文本中提取工具调用
// 如果文本包含 ```tool_call {...} ``` 格式，解析为 OpenAI tool_calls 格式
// 返回值：toolCalls 列表，cleaned 去除 tool_call 块后的文本，hasCalls 是否有工具调用
func ParseToolCalls(text string) (toolCalls []map[string]interface{}, cleaned string, hasCalls bool) {
	matches := toolCallRegex.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, text, false
	}

	for _, m := range matches {
		jsonStr := strings.TrimSpace(m[1])
		var call struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &call); err != nil {
			// 解析失败，跳过这个块
			continue
		}
		if call.Name == "" {
			continue
		}

		argsJSON, _ := json.Marshal(call.Arguments)
		toolCalls = append(toolCalls, map[string]interface{}{
			"id":   "call_" + shortUUID(),
			"type": "function",
			"function": map[string]interface{}{
				"name":      call.Name,
				"arguments": string(argsJSON),
			},
		})
	}

	// 清理文本：移除所有 tool_call 块
	cleaned = toolCallRegex.ReplaceAllString(text, "")
	cleaned = strings.TrimSpace(cleaned)

	return toolCalls, cleaned, len(toolCalls) > 0
}

// BuildToolCallMessage 构建包含 tool_calls 的 assistant message
func BuildToolCallMessage(toolCalls []map[string]interface{}, content string) map[string]interface{} {
	return map[string]interface{}{
		"role":       "assistant",
		"content":    content,
		"tool_calls": toolCalls,
	}
}

// BuildToolResultMessage 构建工具调用结果的 message
func BuildToolResultMessage(toolCallID, content string) map[string]interface{} {
	return map[string]interface{}{
		"role":       "tool",
		"tool_call_id": toolCallID,
		"content":    content,
	}
}
