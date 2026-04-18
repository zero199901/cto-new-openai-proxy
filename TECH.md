# cto.new 逆向技术文档

本文件记录 cto.new 反代实现过程中摸清的全部协议细节、踩坑点、关键字段，便于后续维护 / 二次开发。

## 一、技术栈概览

| 角色 | 服务 | 说明 |
|---|---|---|
| 前端 | Next.js (cto.new) | Vercel 部署 |
| 认证 | Clerk.dev (clerk.cto.new) | OAuth / Email+Password / Magic Link，invisible Turnstile |
| 后端 REST | api.enginelabs.ai | 所有业务端点 |
| 流式 | wss://api.enginelabs.ai | WebSocket token 级流式 |
| 图片存储 | S3 + CloudFront | d1wwbei99h08wh.cloudfront.net |

## 二、Clerk 注册流程

### 2.1 环境配置

GET https://clerk.cto.new/v1/environment 返回：

- captcha_enabled: true, captcha_widget_type: smart (invisible Turnstile)
- captcha_public_key_invisible: `0x4AAAAAAAFV93qQdS0ycilX`
- block_disposable_email_domains: true（禁一次性邮箱，mail.tm 会被拒）

### 2.2 完整注册五步

```
1. GET  /v1/environment                         初始化
2. GET  /v1/client                              拿 client id
3. POST /v1/client/sign_ups                     email+password+captcha_token=smart
      → sua_xxx
4. POST /v1/client/sign_ups/{sua}/prepare_verification  strategy=email_code
      → 发验证邮件
5. POST /v1/client/sign_ups/{sua}/attempt_verification  code=XXXXXX
      → created_session_id=sess_xxx
6. POST /v1/client/sessions/{sess}/touch
```

### 2.3 验证码邮件

- From: `notifications@cto.new`
- Subject: `{6位数字} is your verification code`
- 正则：`^\s*(\d{6})\s+is your verification code`

### 2.4 Turnstile 处理

Clerk 注册页渲染的是 **smart invisible Turnstile**（sitekey `0x4AAAAAAAFV93qQdS0ycilX`）。挑战通过后才会往 `/v1/client/sign_ups` POST。

调用链：

1. Clerk JS 调 `window.turnstile.render(container, {sitekey, callback})` 挂载一个隐藏 widget（挂在 form 里）
2. Widget 在页面 idle / 用户交互时**后台**跑挑战，拿到 token 后 callback 写入 form 的隐藏 input
3. 用户点 Continue → Clerk 的 submit handler 读 token 塞进 `captcha_token` 参数 → POST `/v1/client/sign_ups`

关键约束：

- 没有 token 就**不发 POST**（UI 上只是 loading 旋转，无报错）
- curl 直连永远拿不到 token：`{"code":"captcha_missing_token"}`
- 纯 headless Chrome 很难过：页面要么不渲染表单，要么拿不到 token

### 2.5 Turnstile 风控（自动化的阿喀琉斯之踵）

**同 IP 在短时间内多次自动化尝试**，Cloudflare 会把这个环境标为"已知 bot"，之后**任何**浏览器（包括人工点击）都拿不到 token。表现：

```
[ERROR] Failed to load resource: the server responded with a status of 401 () 
  @ https://challenges.cloudflare.com/cdn-cgi/challenge-platform/h/g/pat/...
[ERROR] Failed to load resource: the server responded with a status of 400 () 
  @ https://challenges.cloudflare.com/cdn-cgi/challenge-platform/h/g/d/...
```

Console 里那一串 `pat/` 和 `d/` 挑战全 401/400 就是铁证。点 Continue 后：

- OAuth 按钮全变 disabled（Clerk 进入"提交中"态）
- 表单 input 消失
- URL 不变，**`/v1/client/sign_ups` POST 从未发出**（Network 面板可验证）

**应对顺位**：

1. **等冷却**（30 分钟 – 数小时，取决于触发频次）
2. **换出口 IP**：`launch(proxy={server: "socks5h://...", username, password})`
3. **`/sign-in` 登录页没有 Turnstile**，已有账号就用 `cto_login_refresh.py` 刷 cookie
4. 实在必要：接 2captcha / CapSolver（付费 token 代解）

**不能解决的方案**：伪造 UA、改 `navigator.webdriver`、等到无人时段。Cloudflare 的指纹检测维度包括 TLS JA4、Canvas/WebGL hash、Performance Timing 抖动等，简单伪装没用。

## 三、Clerk JWT 刷新

### 3.1 JWT 特性

- 有效期 60 秒
- Authorization: Bearer {jwt} 用于所有 api.enginelabs.ai 请求

### 3.2 刷新端点

```
POST https://clerk.cto.new/v1/client/sessions/{sess_id}/tokens
  ?__clerk_api_version=2025-11-10
  &_clerk_js_version=6.7.3
```

**关键点**：必须携带 HTTP-only cookie `__client`，否则返回 401 "Signed out"。

cookie 分布：

| cookie | domain | httpOnly | 用途 |
|---|---|---|---|
| `__client` | .clerk.cto.new | ✅ Yes | **核心** — session 续期的唯一凭据 |
| `__session` | cto.new | ❌ No | 当前短期 JWT |
| `__client_uat` | .cto.new | ❌ No | client activity timestamp |

**取 `__client` 的三种办法**（越往下越稳）：

```python
# ❌ 办法1（Selenium，需先切域）：不稳，有时拿不到
driver.get("https://clerk.cto.new/v1/environment?__clerk_api_version=2025-11-10&_clerk_js_version=6.7.3")
for c in driver.get_cookies():
    if c["name"] == "__client":
        ck = c["value"]

# ✅ 办法2（Selenium + CDP，无需切域）：稳
result = driver.execute_cdp_cmd("Network.getAllCookies", {})
for c in result["cookies"]:
    if c["name"] == "__client" and "clerk" in c["domain"]:
        ck = c["value"]

# ✅ 办法3（Playwright，最干净）
cdp = context.new_cdp_session(page)
data = cdp.send("Network.getAllCookies")
ck = next(c["value"] for c in data["cookies"]
          if c["name"] == "__client" and "clerk" in c["domain"])
```

**注意**：`context.cookies()` / `driver.get_cookies()` 只返回"当前页面关联域"的 cookie，跨域 HTTP-only 必用 CDP 的 `Network.getAllCookies`。

### 3.3 curl 验证

```bash
curl -X POST "https://clerk.cto.new/v1/client/sessions/sess_XXX/tokens?__clerk_api_version=2025-11-10&_clerk_js_version=6.7.3" \
  -H "Origin: https://cto.new" \
  -H "Cookie: __client=eyJhbGci..."
# → 200 {"object":"token","jwt":"eyJhbGci..."}
```

## 四、REST API（api.enginelabs.ai）

### 4.1 账号信息

- `GET /current-user` → 当前用户 + team.apiKey（永久）
- `GET /current-user/status` → 激活状态
- `GET /billing` → tier / dailyUsage / dailyLimit (免费 300) / weeklyLimit (900)
- `GET /billing/tiers` → 所有套餐

### 4.2 Workspace

- `GET /workspaces?pageSize=100`
- `POST /workspaces/create` body: `{name, description}`
- `DELETE /workspaces/{id}`
- `GET /workspaces/{id}/stats`

### 4.3 Project (聊天会话单位)

```
POST /projects/create-hosted
body: {"projectName": "scratch-xxx"}
→ 201
{
  "message": "Project created",
  "projectId": "03719b3f-...",          ⭐ 后续接口用 projectId 作为 chatHistoryId
  "projectShortCode": "dxwpzgkb",
  "workspaceId": "bbdaba5d-...",
  "workspaceName": "scratch-debug-8iw0y2"
}
```

其它：
- `POST /projects/create-expo` (mobile app)
- `POST /projects/create-claw` (CTOClaw agent)
- `POST /projects/create-from-template`
- `GET /project/{id}/secrets`
- `POST /project/{id}/secrets/bulk`

### 4.4 Chat Adapter (模型列表)

```
GET /chat/adapters → { "chatAdapters": [...] }
```

每个 adapter 关键字段：

| 字段 | 含义 |
|---|---|
| `key` | 下划线 snake 形式（gpt_5_4） |
| `model` | 真实模型名（gpt-5.4） |
| `engineAgentKey` | PascalCase（GPT5_4） |
| `provider` | openai / anthropic / google / openrouter / cerebras |
| `minimumTier` | 0=免费, 1/2=付费 |
| `enabled` | 团队是否启用 |
| `usageMultiplier` | 每次调用消耗的配额倍数 |
| `supportsImages` | 是否多模态 |

免费可用：`gpt_5_4` (×3), `glm_5_1` (×2), `gemini3_Flash_Preview` (×5), `gemini3_1_Pro_Preview` (×4), `kimi_k2_5`, `minimax_m2_5`, `devstral_2`, `grok_4_1_fast`。

### 4.5 切换模型

```
PATCH /teams/preferred-model
body: {"modelKey": "gpt_5_4"}         ⚠️ 字段名是 modelKey，不是 model
```

### 4.6 发消息（三连）

```
1) POST /engine-agent/offers
   body: {"chatHistoryId": "<projectId>", "surface": "ENGINE_AGENT"}
   → 200 {"offers":[...]}   （广告 slot，可忽略，但必须调）

2) POST /engine-agent/chat                                    ⭐ 异步提交
   body: {
     "prompt": "<base64(user_text)>",
     "chatHistoryId": "<projectId>",
     "encoded": true,
     "imageUrls": ["https://d1wwbei99h08wh.cloudfront.net/{uuid}.png"]  // 可选
   }
   → 202 Accepted (空 body)
```

> 服务端异步处理，响应通过下面的 WebSocket 推回来。

### 4.7 聊天历史

- `GET /engine-agent/chat-histories?workspaceId=xxx&pageSize=5`
- `DELETE /engine-agent/chat-histories/{chatHistoryId}`
- `GET /engine-agent/chat/{chatHistoryId}/mcps`

## 五、WebSocket 流式协议

### 5.1 连接 URL

```
wss://api.enginelabs.ai/engine-agent/chat-histories/{chatHistoryId}/buffer/stream
  ?token={userId}:{orgId}
  &workspaceId={workspaceId}
```

- **URL 参数里的 `token` 是 `userId:orgId` 明文组合**（不是 JWT！）
- 建立连接需要 `__client` cookie + Origin: `https://cto.new`

### 5.2 帧类型

| type | 含义 | 触发时机 |
|---|---|---|
| `history` | 完整历史快照 | 连接建立后立刻推 |
| `state` | 任务状态 `{inProgress, cancelled, taskId}` | 开始 / 结束 |
| `update` | **增量消息**（token 级或完整消息） | 生成中 |
| `label` | 对话自动命名 | 首条回复后 |
| `"ping"` / `"pong"` | 心跳（客户端→服务端 "ping"，回 "pong"） | 每 30 秒 |

### 5.3 update 帧详解

`buffer` 字段是**字符串化的 JSON**（需二次解析）：

```
// 用户消息回显
{"type":"update","buffer":"{\"id\":\"1a931d50-...\",\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"...\"}]}\n"}

// assistant token 增量
{"type":"update","buffer":"{\"id\":\"msg_1776411902994_0\",\"type\":\"chat\",\"chat\":{\"content\":\"pong\"}}\n"}
```

解析策略：
1. 取 outer `type==update`
2. 取 `buffer` 字符串，再 json.parse
3. 看 inner 对象：
   - `role==user` → 忽略（是 user 消息 echo）
   - `type==chat` → 读 `chat.content` 作为 delta

**同一条 assistant 消息的 update 帧，`id` 字段相同**（如 `msg_1776411902994_0`），客户端拼接所有 content 得到完整回复。

### 5.4 结束信号

连续收到 `{"type":"state","state":{"inProgress":false}}` 即可视为本次生成结束。

## 六、图片上传与识别

### 6.1 上传三步

```
1) POST /images/presign
   body: {"images": [{"mediaType": "image/png", "contentLength": 4102}]}
   → 返回 presigned S3 URL 列表

2) PUT <S3 presigned URL>
   body: 原始图片字节
   → 200 OK

3) POST /engine-agent/chat 携带 imageUrls
   body: {
     "prompt": "<base64>",
     "chatHistoryId": "...",
     "encoded": true,
     "imageUrls": ["https://d1wwbei99h08wh.cloudfront.net/{uuid}.png"]
   }
```

### 6.2 实测

已在浏览器环境验证 GPT-5.4 能识别自制测试图（红色圆 + "CTO-TEST" 文字），回复：`Red circle, CTO-TEST`。

### 6.3 支持情况

- `gpt_5_4`: `supportsImages: true`  ✅
- `glm_5_1`: `supportsImages: false`  ❌
- `claude-opus/sonnet`: `supportsImages: true`
- `gemini-*-pro`: `supportsImages: true`

## 七、账号配额

Free tier：

| 指标 | 上限 | 周期 |
|---|---|---|
| dailyLimit | 300 | 日 |
| weeklyLimit | 900 | 周 |
| taskConcurrencyLimit | 2 | 并发 |

每次调用按 `usageMultiplier` 扣：

- GPT 5.4: ×3 → 100 次/天 / 300 次/周
- GLM 5.1: ×2 → 150 次/天 / 450 次/周
- Gemini 3 Flash: ×5

用尽后 `GET /billing` 返回 `hasReachedLimit: true`，对应的 `limitReached` 含 retry 时间。

## 八、核心踩坑点

### 8.1 `__client` HTTP-only

```
错误：document.cookie 只能拿到 __session（短期 JWT）
正解：Selenium driver.get("https://clerk.cto.new/...") + get_cookies() 
     可以取到 httpOnly 的 __client cookie。
     这是反代能持续刷新 JWT 的唯一方式。
```

### 8.2 chatHistoryId 来源

```
错误：以为 /projects/create-hosted 响应里有 chatHistoryId
正解：响应只返回 projectId，但 projectId 在 /engine-agent/offers 和 
     /engine-agent/chat 里**直接当作 chatHistoryId 使用**。
```

### 8.3 PATCH /teams/preferred-model 字段名

```
错误：body: {"model": "gpt_5_4"}      → 500 "body must have required property modelKey"
正解：body: {"modelKey": "gpt_5_4"}
```

### 8.4 prompt base64 编码

```
/engine-agent/chat 必须 base64 编码 prompt 并设 encoded=true。
不 base64 时中文 / 特殊符号会丢字。
```

### 8.5 tempmail.plus vs mail.tm

```
mail.tm 所有域都被 Clerk 标为 disposable，注册时直接 DISPOSABLE_EMAIL 拒绝。
tempmail.plus 的 `<your-subdomain>@mailto.plus` + 任意子前缀 `({idx}@<your-subdomain>.email)` 不会被标。
```

## 九、端到端测试记录

### 9.1 注册

用 Playwright 自动化注册 （示例邮箱 `1@example.email`）：

```
1. 打开 https://accounts.cto.new/sign-up
2. 填 email + 随机密码 + 勾选 TOS
3. Continue → Clerk invisible Turnstile 自动过
4. 跳转 /sign-up/verify-email-address
5. 从 tempmail.plus 拿 6 位验证码（"890325 is your verification code"）
6. 填入 → 自动跳回 https://cto.new/
7. window.Clerk 已 ready，拿到 session JWT
```

### 9.2 发送消息（浏览器）

GPT-5.4 提示 "Count from 1 to 20 separated by commas"，WebSocket 收到 **59 个 update 帧**，token 级分片：

```
1 , (空格) 2 , (空格) 3 , ... , (空格) 20
```

### 9.3 反代测试（Go）

| 测试 | 请求 | 响应 |
|---|---|---|
| OpenAI GPT-5.4 流式 | `pong-from-gpt54` | 6 个 delta chunk + [DONE] |
| OpenAI GPT-5.4 非流式 | `2+2=?` | `"content":"4"` |
| Anthropic GLM-5.1 流式 | `hello-glm51` | 完整 Anthropic SSE |

## 十、端点清单速查

### Clerk (clerk.cto.new)

| Method | Path | 用途 |
|---|---|---|
| GET | /v1/environment | 配置 |
| GET | /v1/client | 初始化 client |
| POST | /v1/client/sign_ups | 注册 |
| POST | /v1/client/sign_ups/{sua}/prepare_verification | 发邮件 |
| POST | /v1/client/sign_ups/{sua}/attempt_verification | 提交验证码 |
| POST | /v1/client/sign_ins | 登录 |
| POST | /v1/client/sessions/{sid}/tokens | **刷 JWT** |
| POST | /v1/client/sessions/{sid}/touch | 选 session |

### Engine Labs (api.enginelabs.ai)

| Method | Path | 用途 |
|---|---|---|
| GET | /current-user | 用户信息 + team.apiKey |
| GET | /billing | 余量 |
| GET | /chat/adapters | 模型列表 |
| PATCH | /teams/preferred-model | **切模型** (modelKey) |
| POST | /projects/create-hosted | 新建项目 |
| POST | /engine-agent/offers | 发消息前的 prelude |
| POST | /engine-agent/chat | **发消息** (prompt base64) |
| POST | /images/presign | 图片上传 presign |
| GET | /engine-agent/chat-histories | 历史列表 |
| GET | /engine-agent/chat/{id}/mcps | MCP 配置 |
| WS | /engine-agent/chat-histories/{id}/buffer/stream | **接流** |

## 十一、调试工具

### 查看所有 cookies（含 HTTP-only）

Playwright：

```js
const cookies = await page.context().cookies();
```

Selenium：

```python
# 1. 先导航到目标域
driver.get("https://clerk.cto.new/v1/environment?...")
# 2. get_cookies 返回该域所有 cookie
cookies = driver.get_cookies()
```

### 拦截 WebSocket

Playwright：

```js
page.on('websocket', ws => {
  ws.on('framereceived', f => console.log('recv:', f.payload.toString()));
  ws.on('framesent', f => console.log('sent:', f.payload.toString()));
});
```

### JWT 解析 (不校验签名)

```python
import base64, json
def parse_jwt(t):
    p = t.split('.')[1]
    p += '=' * (-len(p) % 4)
    return json.loads(base64.urlsafe_b64decode(p))
```

### curl 直接测刷 JWT

```bash
export CLIENT='eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...'
export SID='sess_XXX'
curl -X POST "https://clerk.cto.new/v1/client/sessions/$SID/tokens?__clerk_api_version=2025-11-10&_clerk_js_version=6.7.3" \
  -H "Origin: https://cto.new" \
  -H "Cookie: __client=$CLIENT"
```

## 十二、文件组织

```
cto.new注册反代/
├── README.md                 快速上手
├── TECH.md                   本文件
├── accounts.jsonl            每行一个账号 JSON（go-proxy 热加载）
├── cto_register.py           Selenium 注册（默认 --no-headless，--headless 可切）
├── cto_register_pw.py     ⭐ Playwright 注册（推荐，反检测更好）
├── cto_login_refresh.py   ⭐ Cookie 失效时自动登录刷 __client（不经 Turnstile）
└── go-proxy-cto/
    ├── config.go             ⭐ ModelMap: 对外 model 名 → engineKey
    ├── clerk.go              ⭐ 用 __client cookie 刷 JWT
    ├── account.go / pool.go  账号池 + 热加载 + 配额标记
    ├── engine.go             create-hosted / offers / chat / preferred-model
    ├── ws_engine.go          ⭐ WebSocket 解析 update 帧
    ├── stream.go             发消息总控：切模型 → 建项目 → 接流
    ├── handler_openai.go     /v1/chat/completions
    ├── handler_anthropic.go  /v1/messages
    ├── homepage.go           Dashboard (/)
    ├── main.go
    └── cto-proxy          ⭐ 已编译 mac/arm64 二进制
```

## 十三、Selenium vs Playwright —— 注册场景

| 维度 | Selenium | Playwright |
|---|---|---|
| `navigator.webdriver` 默认暴露 | ✅ 暴露（需手工 patch） | ❌ Playwright 已内置隐藏 |
| TLS / HTTP/2 指纹 | 跟系统 Chrome 一致 | Chromium 指纹（和真实 Chrome 略有差异，但通过率更高） |
| 真实 input 事件模拟 | send_keys 逐键 | fill 一次性 + Keyboard 链路更真实 |
| CSS `[attr]` selector | **某些版本下 WebDriverWait 不识别** | 原生 Locator 全支持 |
| page_load_strategy="eager" | 需手工设 | 默认 |
| Cloudflare Turnstile 通过率 | 低（常被 block） | 中（新 IP 下基本能过） |
| 调试 DOM | execute_script | `page.on('console')` / `evaluate` 更方便 |

**推荐**：用 `cto_register_pw.py`（Playwright），备 `cto_register.py`（Selenium）。

### 13.1 Selenium 踩坑记录（已在脚本里修掉）

| 现象 | 根因 | 修法 |
|---|---|---|
| `input[type="email"]` 找不到 | Clerk 用 `type="text"` | 改 `By.ID, "emailAddress-field"` |
| `WebDriverWait(By.ID,...)` 超时但 JS 能找到 | Selenium 的 `find_element` 被 pageload 阻塞 | `page_load_strategy="eager"` + 改用 JS 填表 |
| headless=new 下 Clerk 不渲染 | Clerk 检测 headless 直接屏蔽 UI | 默认 `--no-headless`（真 Chrome 窗口） |
| 伪造 `navigator.plugins=[1,2,3,4,5]` 后表单空白 | 值太假触发更严指纹 | 只覆盖 `navigator.webdriver` 一个字段 |
| Chrome 130/132 UA 老版本卡住 | UA 和真实 Chrome major 不匹配 | 固定为真实 `Chrome/147.x.x` |
| `prefs.credentials_enable_service=False` 等 flag 干扰 | 多 flag 触发指纹异常 | 砍到只留 no-sandbox / UA / window-size |

### 13.2 React controlled input 填表

Clerk 用 React controlled input，直接 `elem.value = ...` **不触发 React state 更新**。正确方法：

```js
function setNativeValue(el, value) {
  const proto = Object.getPrototypeOf(el);
  const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
  setter.call(el, value);                              // 调 native setter
  el.dispatchEvent(new Event('input',  { bubbles: true }));
  el.dispatchEvent(new Event('change', { bubbles: true }));
}
```

## 十四、登录流程（sign-in，无 Turnstile）

`cto_login_refresh.py` 基于这个流程：

```
1. GET  /v1/environment                                     client 初始化
2. POST /v1/client/sign_ins                                 identifier + password
      → {"response":{"id":"sia_xxx","status":"needs_first_factor",...}}
3. POST /v1/client/sign_ins/{sia}/prepare_first_factor      strategy=password
4. POST /v1/client/sign_ins/{sia}/attempt_first_factor      strategy=password, password=...
      → {"response":{"created_session_id":"sess_xxx","status":"complete"}}
5. POST /v1/client/sessions/{sess}/touch                    激活
```

**没有 Turnstile 步骤**，所以登录在 IP 被风控时仍 100% 可用。

登录完成后所有 cookie 已种（含新的 `__client` with 新 rotating_token），Playwright 直接从 CDP 拿。

### 14.1 登录页 selector

| 字段 | 选择器 |
|---|---|
| email input | `#identifier-field` (type=text, name=identifier) |
| password input | `#password-field` (type=password) |
| 提交按钮 | `button:visible:has-text("Continue")` / `.cl-formButtonPrimary` |

注册页是 `#emailAddress-field`，登录页是 `#identifier-field`——Clerk 故意不一样。

## 十五、后续扩展思路

1. **图像接入 OpenAI 格式**：OpenAI 的 messages 支持 `{"type":"image_url","image_url":{"url":"..."}}`，在 handler_openai.go 里检测这种块，下载图片 → `/images/presign` → S3 PUT → 填 `imageURLs` 参数给 SendChatAndStream。
2. **Anthropic tool_use**：cto.new agent 有自己的 tool 协议（`type:tool_call` update 帧），需要做映射层。可参考原 `anything注册机/go-proxy/handler_agent.go` 三阶段逻辑（initial / analyze / success）。
3. **配额主动探测**：每 N 次请求后调 `GET /billing`，若 `hasReachedLimit` 则主动 `pool.Release(acc, "daily limit exceeded")`。
4. **多账号并发**：目前每个请求独占一个账号；可以改成 `acc.inUse` 不阻塞，同账号支持多个并发 WS（实测 cto.new 允许同账号多 WS）。
5. **代理池支持**：为 `cto_register_pw.py` 加 `--proxy` 参数，每次注册随机挑一个 SOCKS5 出口，能绕过 Turnstile 风控。
6. **自动刷新 daemon**：`cto_login_refresh.py` 做成定时任务（每 6 小时跑一次 `--all`），彻底不用手工维护账号池。
