package main

import (
	"sync"
	"time"
)

// ============================================================
// 请求日志 —— 内存环形缓冲区，保留最近 N 条请求记录
// ============================================================

const MaxLogEntries = 500

// LogEntry 单条请求日志
type LogEntry struct {
	Time      string `json:"time"`       // ISO8601
	Method    string `json:"method"`     // GET / POST
	Path      string `json:"path"`       // 请求路径
	Model     string `json:"model"`      // 请求的模型名
	Account   string `json:"account"`    // 使用的账号邮箱
	Status    int    `json:"status"`     // HTTP 状态码
	Duration  int64  `json:"duration"`   // 耗时毫秒
	Stream    bool   `json:"stream"`     // 是否流式
	Error     string `json:"error"`      // 错误信息（空=成功）
	RemoteIP  string `json:"remote_ip"`  // 客户端 IP
	TokensIn  int    `json:"tokens_in"`  // 输入 token 数（估算）
	TokensOut int    `json:"tokens_out"` // 输出 token 数（估算）
}

// RequestLog 环形缓冲区
type RequestLog struct {
	mu      sync.RWMutex
	entries []LogEntry
	head    int // 下一条写入位置
	size    int // 当前条数
}

var globalLog = &RequestLog{
	entries: make([]LogEntry, MaxLogEntries),
}

// Add 添加一条日志
func (l *RequestLog) Add(entry LogEntry) {
	l.mu.Lock()
	l.entries[l.head] = entry
	l.head = (l.head + 1) % MaxLogEntries
	if l.size < MaxLogEntries {
		l.size++
	}
	l.mu.Unlock()
}

// Recent 返回最近 n 条日志（按时间倒序）
func (l *RequestLog) Recent(n int) []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n > l.size {
		n = l.size
	}
	out := make([]LogEntry, 0, n)
	// head-1 是最新条目，往前取 n 条
	for i := 0; i < n; i++ {
		idx := (l.head - 1 - i + MaxLogEntries) % MaxLogEntries
		out = append(out, l.entries[idx])
	}
	return out
}

// Stats 返回日志统计
func (l *RequestLog) Stats() map[string]interface{} {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var totalDur int64
	var success, fail int
	var totalTokensIn, totalTokensOut int
	for i := 0; i < l.size; i++ {
		e := l.entries[(l.head-1-i+MaxLogEntries)%MaxLogEntries]
		// 只统计最近 24 小时
		if t, err := time.Parse(time.RFC3339, e.Time); err == nil {
			if time.Since(t) > 24*time.Hour {
				continue
			}
		}
		totalDur += e.Duration
		if e.Error == "" {
			success++
		} else {
			fail++
		}
		totalTokensIn += e.TokensIn
		totalTokensOut += e.TokensOut
	}
	return map[string]interface{}{
		"total_24h":       success + fail,
		"success_24h":     success,
		"fail_24h":        fail,
		"avg_duration_ms": avgDuration(totalDur, success+fail),
		"tokens_in_24h":   totalTokensIn,
		"tokens_out_24h":  totalTokensOut,
	}
}

func avgDuration(total int64, count int) int64 {
	if count == 0 {
		return 0
	}
	return total / int64(count)
}
