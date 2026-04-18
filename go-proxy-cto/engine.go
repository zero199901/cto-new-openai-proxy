package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"
)

// engineClient 访问 api.enginelabs.ai 的轻量 HTTP 封装
type engineClient struct {
	http  *http.Client
	clerk *clerkClient
}

func newEngineClient(ck *clerkClient) *engineClient {
	return &engineClient{
		http:  &http.Client{Timeout: 30 * time.Second},
		clerk: ck,
	}
}

// do 带自动 JWT 刷新的 REST 调用
// method: GET/POST/PATCH/DELETE
// body:   nil 或者可序列化为 JSON 的对象
// 返回: HTTP 状态 + body bytes + err
func (e *engineClient) do(a *Account, method, path string, body interface{}) (int, []byte, error) {
	if err := e.clerk.EnsureFreshJWT(a); err != nil {
		return 0, nil, fmt.Errorf("ensure jwt: %w", err)
	}

	var reader io.Reader
	if body != nil {
		j, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, EngineAPI+path, reader)
	if err != nil {
		return 0, nil, err
	}
	a.mu.Lock()
	jwt := a.JWT
	a.mu.Unlock()
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://cto.new")
	req.Header.Set("Referer", "https://cto.new/")
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	// 401/403：Clerk JWT 失效（被服务端拉黑、时钟偏移、cookie 被 rotate 后 refresh 失败等）
	// 本层主动标记账号为 dead，避免下次请求又复用同一把坏 cookie 继续 401。
	// 注意：pool.Release 也会在错误文本里找 "401/unauthorized"，本层属于双保险。
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		a.MarkDead()
	}
	return resp.StatusCode, b, nil
}

// setPreferredModel 切换当前团队默认模型（影响下一条对话走哪个 adapter）
// 注意：请求体必须用 modelKey 而不是 model
func (e *engineClient) setPreferredModel(a *Account, engineKey string) error {
	st, body, err := e.do(a, "PATCH", "/teams/preferred-model", map[string]string{
		"modelKey": engineKey,
	})
	if err != nil {
		return err
	}
	if st != 200 && st != 204 {
		return fmt.Errorf("set model HTTP %d: %s", st, truncate(string(body), 200))
	}
	return nil
}

// HostedProjectResp /projects/create-hosted 的响应（只取我们用的字段）
type HostedProjectResp struct {
	ProjectID      string `json:"projectId"`
	ChatHistoryID  string `json:"chatHistoryId"`
	WorkspaceID    string `json:"workspaceId"`
	// 有可能还有 repositoryId / url 等；用 json.RawMessage 忽略
}

// createHostedProject 新建一个临时 hosted project
// 服务端响应只含 projectId + workspaceId + projectShortCode + workspaceName
// 关键：projectId 在后续 /engine-agent/offers 和 /engine-agent/chat 里当 chatHistoryId 使用
func (e *engineClient) createHostedProject(a *Account, name string) (*HostedProjectResp, error) {
	st, body, err := e.do(a, "POST", "/projects/create-hosted", map[string]string{
		"projectName": name,
	})
	if err != nil {
		return nil, err
	}
	if st != 200 && st != 201 {
		return nil, fmt.Errorf("create project HTTP %d: %s", st, truncate(string(body), 300))
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("create project json: %w", err)
	}
	r := &HostedProjectResp{}
	r.ProjectID = firstString(m, "projectId", "project_id", "id")
	r.WorkspaceID = firstString(m, "workspaceId", "workspace_id")
	r.ChatHistoryID = firstString(m, "chatHistoryId", "chat_history_id")
	// cto.new 的 create-hosted 不返回 chatHistoryId —— projectId 本身就是 chat history id
	if r.ChatHistoryID == "" {
		r.ChatHistoryID = r.ProjectID
	}
	if r.ChatHistoryID == "" || r.WorkspaceID == "" {
		return nil, fmt.Errorf("create project: missing ids in response: %s",
			truncate(string(body), 300))
	}
	return r, nil
}

// sendPrompt POST /engine-agent/chat：把用户 prompt base64 后异步提交
// imageURLs 可选（带图时用 CloudFront URL，由 presignAndUpload 返回）
func (e *engineClient) sendPrompt(a *Account, chatHistoryID, prompt string, imageURLs []string) error {
	// base64 以避免内部字符冲突，协议要求 encoded=true
	b64 := base64.StdEncoding.EncodeToString([]byte(prompt))
	payload := map[string]interface{}{
		"prompt":        b64,
		"chatHistoryId": chatHistoryID,
		"encoded":       true,
	}
	if len(imageURLs) > 0 {
		payload["imageUrls"] = imageURLs
	}

	st, body, err := e.do(a, "POST", "/engine-agent/chat", payload)
	if err != nil {
		return err
	}
	// 协议里成功是 202 Accepted，body 为空
	if st != 202 && st != 200 {
		return fmt.Errorf("send prompt HTTP %d: %s", st, truncate(string(body), 200))
	}
	return nil
}

// sendOffers 发 prompt 前一步：POST /engine-agent/offers
func (e *engineClient) sendOffers(a *Account, chatHistoryID string) error {
	st, body, err := e.do(a, "POST", "/engine-agent/offers", map[string]string{
		"chatHistoryId": chatHistoryID,
		"surface":       "ENGINE_AGENT",
	})
	if err != nil {
		return err
	}
	if st < 200 || st >= 300 {
		return fmt.Errorf("offers HTTP %d: %s", st, truncate(string(body), 200))
	}
	return nil
}

// genProjectName 随机生成一个 hosted 项目名，无需对用户展示
func genProjectName() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return "scratch-" + string(b)
}

// firstString 从 map 里按优先级取第一个非空字符串
func firstString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}
