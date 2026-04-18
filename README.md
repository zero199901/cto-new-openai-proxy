# cto.new Free API Proxy · cto.new 免费反代

**Batch-register cto.new accounts and expose GPT-5.4 / GLM-5.1 as free OpenAI & Anthropic compatible APIs.**
**批量注册 [cto.new](https://cto.new) 账号，把 GPT-5.4 / GLM-5.1 免费模型以 OpenAI `/v1/chat/completions` 和 Anthropic `/v1/messages` 兼容接口的形式暴露出来。**

[English](#english) · [中文](#中文)

---

## English

### What it does

A Go reverse proxy that turns [cto.new](https://cto.new) (Engine Labs) free chat models into standard **OpenAI** / **Anthropic** compatible APIs, with automatic account rotation from a bulk-registered pool.

- **Models**: `gpt-5.4` (3× quota), `glm-5.1` (2× quota), plus aliases (`gpt-4o`, `claude-sonnet-4.6`, `o1/o3` → all route to `gpt_5_4`)
- **Protocols**: OpenAI chat completions (stream + non-stream), Anthropic messages (stream + non-stream), multimodal input
- **Account pool**: LRU selection, auto JWT refresh via Clerk `__client` cookie, quota/dead marking with 6h cooldown, 30s hot-reload of `accounts.jsonl`
- **Dashboard**: Web UI at `http://127.0.0.1:9091` with tabs for Overview, Accounts, Request Logs, Usage
- **Bulk registration**: Playwright / Selenium scripts to create accounts, plus a login-based cookie refresher for when `__client` expires

### ⚠️ Important: No agent / function calling support

This proxy **does not** support OpenAI function calling or Anthropic `tool_use`. Reason: cto.new's `/engine-agent/chat` endpoint is itself a full Engine Labs Coding Agent with its own sandbox and system prompt — we can't hijack tool-call protocol at the proxy layer. Even with OpenAI-standard `tools` field injection + aggressive prompt engineering, the upstream model still follows its own agent logic and won't emit structured `tool_calls`.

**Good fit** ✅
- Plain chat, code explanation / generation, documentation, translation
- `Plan` mode in opencode / cline / continue.dev (read-only discussion, no file edits)
- Quick LLM output quality testing

**Poor fit** ❌
- Build / Agent mode in opencode / cline (requires `read_file` / `bash` / `edit_file` tools)
- Claude Code / Continue auto-coding tasks
- Any pipeline depending on function calling

For tool calling, use providers that natively support it (official OpenAI / Anthropic / various relay APIs).

### Quick start

```bash
# 1. Clone
git clone https://github.com/1837620622/cto-new-openai-proxy.git
cd cto-new-openai-proxy

# 2. Register accounts (Playwright, first-run shows browser)
pip3 install playwright requests
playwright install chromium
# ⚠️ Edit cto_register.py: set EMAIL_DOMAIN and TEMPMAIL_RECEIVER
#    to your own tempmail.plus subdomain, or export env vars:
#    export CTO_EMAIL_DOMAIN=yoursub.email
#    export CTO_TEMPMAIL_RECEIVER=yoursub@mailto.plus
python3 cto_register_pw.py --start 1 --count 5 --show

# 3. Build & run the Go proxy
cd go-proxy-cto
go build -o cto-proxy .
cd ..
CTO_BASE_DIR="$(pwd)" ./go-proxy-cto/cto-proxy
# Dashboard → http://127.0.0.1:9091
```

### Usage examples

**OpenAI compatible**
```bash
curl -N http://127.0.0.1:9091/v1/chat/completions \
  -H 'Authorization: Bearer xs2-cto-secret' \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hi"}]}'
```

**Anthropic compatible**
```bash
curl -N http://127.0.0.1:9091/v1/messages \
  -H 'x-api-key: xs2-cto-secret' -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{"model":"glm-5.1","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}'
```

Change the API key via `PROXY_API_KEY` env var. Default listen address `127.0.0.1:9091` is set in `go-proxy-cto/config.go`.

### Admin endpoints

| Path | Description |
|---|---|
| `GET /` | Dashboard UI |
| `GET /status` | Pool stats JSON |
| `GET /v1/models` | Supported model list |
| `GET /admin/accounts` | Full account state (requires API key) |
| `GET /admin/logs?n=100` | Recent request logs |
| `GET /admin/log-stats` | 24h aggregated stats |
| `POST /admin/reload` | Hot-reload `accounts.jsonl` |

### Limitations

- **Turnstile friction**: the `/sign-up` page uses Cloudflare invisible Turnstile; same-IP bulk registration will be throttled (401). Use `cto_login_refresh.py` to recover via `/sign-in` (no Turnstile).
- **Quota**: each free account gets ~300 requests/day. With a 7-account pool that's ≈700/day for `gpt-5.4` (×3 multiplier) or ≈1050/day for `glm-5.1` (×2).
- **No native tool use** (see warning above).
- **Message size**: upstream caps at ~32 KB body. The proxy truncates prompts > 20 KB to keep system prompt + most recent turns.

### Tech stack

- **Go 1.21+** with Gin, gorilla/websocket, zap
- **Python 3.10+** with Playwright, Selenium, SeleniumBase UC mode, requests
- **Clerk** authentication (`__client` cookie → session JWT via `POST /v1/client/sessions/{sid}/tokens`)
- **Engine Labs** upstream: `POST /projects/create-hosted` → `POST /engine-agent/offers` → `POST /engine-agent/chat` → WebSocket stream

See [TECH.md](TECH.md) for protocol details, field-by-field reverse engineering notes, and debugging stories.

### License

MIT. Use at your own risk. Respect cto.new's ToS.

---

## 中文

把 [cto.new](https://cto.new) 的 **GPT-5.4** 和 **GLM-5.1** 免费模型，批量注册拿账号后，以 Go 反代形式暴露成 **OpenAI `/v1/chat/completions`** 和 **Anthropic `/v1/messages`** 兼容接口。

> ✅ 已跑通端到端流式 + 非流式 + 图像识别。账号池自动 JWT 刷新、配额标记、热加载。
> ⚠️ Clerk 注册页有 **Cloudflare invisible Turnstile** —— 同 IP 短时间多次自动化会触发风控（401），需冷却或换 IP，详见 TECH.md。

## ⚠️ 重要声明：Agent / 工具调用能力

**本反代不具备 Agent 工具调用能力（不支持 function calling / tool use）。**

原因：cto.new 的 `/engine-agent/chat` 端点本身就是完整的 Engine Labs Coding Agent，它有自己的 system prompt 和沙箱环境，我们无法在反代层面接管工具调用协议。即使传入 OpenAI 标准 `tools` 参数并在 prompt 中注入强指令，上游模型仍会按它自己的 agent 逻辑响应，而不会输出结构化的 `tool_calls`。

**适用场景** ✅
- 纯对话、代码解释、代码生成、文档写作、翻译
- 在 opencode / cline / continue.dev 中做 **Plan 模式**（只读讨论，不改文件）
- 快速测试 LLM 输出质量

**不适用场景** ❌
- opencode / cline 的 Build/Agent 模式（需要 read_file/bash/edit_file 等工具）
- Claude Code / Continue 的自动编程任务
- 任何依赖 function calling 的 pipeline

如果需要工具调用能力，请使用支持原生 function calling 的供应商（官方 OpenAI / Anthropic / 各类中转 API）。

## 目录结构

```
cto.new注册反代/
├── README.md                    本文件
├── TECH.md                      详细技术文档（协议、字段、调试、Turnstile 风控）
├── accounts.jsonl               账号池（每行一个 JSON 账号）
├── cto_register.py              Selenium 批量注册机（已保留）
├── cto_register_pw.py           ⭐ Playwright 批量注册机（推荐，反检测更好）
├── cto_login_refresh.py         ⭐ Cookie 失效时自动登录刷 __client（不经 Turnstile）
└── go-proxy-cto/                Go 反代
    ├── main.go                  入口 + 路由
    ├── config.go                端点常量 + 模型映射
    ├── account.go               账号结构
    ├── pool.go                  账号池 / 热加载 / 配额标记
    ├── clerk.go                 Clerk JWT 自动刷新
    ├── engine.go                api.enginelabs.ai REST 客户端
    ├── ws_engine.go             WebSocket 流式接入
    ├── stream.go                统一发消息 + 流转换入口
    ├── handler_openai.go        /v1/chat/completions
    ├── handler_anthropic.go     /v1/messages
    ├── homepage.go              简易 Dashboard
    ├── cto-proxy                ⭐ 已编译的 mac/arm64 二进制（14 MB）
    └── go.mod
```

## 核心工作流

```
[Playwright 注册] ──→ accounts.jsonl ──→ [Go 反代启动]
                                          ├─ Clerk JWT 自动刷新（__client cookie）
                                          ├─ POST /engine-agent/chat
                                          ├─ WebSocket 收 token 流
                                          └─ 转成 OpenAI / Anthropic SSE

[Cookie 失效] ──→ cto_login_refresh.py ──→ [登录抓新 __client]
                 （走 /sign-in，无 Turnstile）
```

## 一、批量注册

### 1.1 依赖

```bash
# Playwright 版（推荐）
pip3 install playwright requests
playwright install chromium

# Selenium 版（备用，Clerk Turnstile 对 Selenium 识别更严）
pip3 install selenium requests
```

### 1.2 邮箱与验证码

脚本支持多种邮箱模式：

- **`tempmail.plus`**：使用 `{序号}@<your-subdomain>.email` 格式，所有邮件投递到你的 `<your-subdomain>@mailto.plus` 收件。使用前请修改 `cto_register.py` 的 `EMAIL_DOMAIN` 和 `TEMPMAIL_RECEIVER` 常量或用环境变量覆盖。
- **`gptmail`**（新版 SeleniumBase 脚本）：随机递增邮箱，无需配置。

Clerk 的验证码在**邮件主题**里：`XXXXXX is your verification code`，脚本会正则提取 6 位数字。

> 提示：`mail.tm` 等一次性邮箱被 Clerk 标为 disposable 直接拒绝。实测 `tempmail.plus` + 自定义子前缀的格式 未被拦截。

### 1.3 运行（Playwright 版，推荐）

```bash
# 进入项目根目录（下面以仓库根目录为工作目录）
cd cto.new注册反代

# 显示浏览器便于观察 Turnstile 行为（首次强烈推荐）
python3 cto_register_pw.py --start 2 --count 1 --show

# 批量注册 5 个（headless）
python3 cto_register_pw.py --start 2 --count 5

# 每次注册之间留 4-9 秒随机间隔（已内置）
```

### 1.4 运行（Selenium 版，备用）

```bash
# 默认非 headless（更稳），加 --headless 才切 headless
python3 cto_register.py --start 2 --count 1
python3 cto_register.py --start 2 --count 5 --headless
```

### 1.5 Turnstile 风控 & 应对

Clerk 用 Cloudflare invisible Turnstile 防 bot，**同 IP 短时间多次自动化会被拉黑**（表现：点 Continue 后 /v1/client/sign_ups 根本不发出，console 里一堆 `challenges.cloudflare.com … 401`）。

- **检测**：`curl -s -w '%{http_code}\n' https://accounts.cto.new/sign-up` 回 200 只说明页面能加载，不代表 Turnstile 通。
- **冷却**：通常 30 分钟 – 数小时，关闭并过一阵再试
- **换 IP**：设 Chrome 代理（Playwright `launch(proxy={...})` 或 `--proxy-server=`）
- **人工介入**：`--show` 模式打开浏览器，出现挑战时用户自己点一下（但 invisible 没 UI，故此路难行）
- **Fallback**：若已有账号能登录但 cookie 过期，用 `cto_login_refresh.py` 走 /sign-in（登录页**没有 Turnstile**，几乎 100% 成功）

### 1.6 注册成功后立即写入

每成功一个立即 append 到 `accounts.jsonl`，Go 反代每 30 秒自动热加载。脚本对注册成功后的 cookie 做一次 `verify_refresh()` 自检（实际调 Clerk refresh 换 JWT），过不了就不落盘，避免"看着成功其实没用"的坏账号。

### 1.7 accounts.jsonl 完整字段

每个账号对象包含：

| 字段 | 生成 | 用途 |
|---|---|---|
| `email` | 注册输入 | `{idx}@<your-domain>` |
| `password` | 脚本随机 | `cto_login_refresh.py` 登录时用 |
| `clerk_user_id` | `window.Clerk.user.id` | Clerk 用户 ID（`user_xxx`） |
| `clerk_session_id` | `window.Clerk.session.id` | **JWT 刷新 URL 的一部分** |
| `clerk_org_id` | `session.lastActiveOrganizationId` | Clerk 组织 ID（`org_xxx`） |
| `clerk_client_cookie` | CDP `Network.getAllCookies` | **⭐ HTTP-only `__client`，刷新 JWT 的唯一凭据（1 年有效）** |
| `clerk_session_cookie` | 同上 | 启动时的 60s JWT（冗余，反代会用 `__client` 重刷） |
| `clerk_client_uat` | 同上 | Clerk client 创建时间戳（辅助刷新） |
| `engine_user_id` | `/current-user` 响应 | Engine Labs 用户 UUID |
| `engine_team_id` | `/current-user` 响应 | ⭐ **WebSocket token 里用** |
| `engine_api_key` | `/current-user` 响应 | Engine API Key（保留给未来用） |
| `default_workspace_id` | `/workspaces` 列表 | 备用（反代每次自建 workspace） |
| `registered_at` | 脚本写入 | ISO8601 时间戳 |
| `ws_token_suffix` | 拼接 | `{clerk_user_id}:{clerk_org_id}`，WebSocket `?token=` 的内容 |

注册脚本的自检流程确保 `clerk_client_cookie` + `clerk_session_id` 能实际刷 JWT 后才落盘。

## 二、Go 反代

### 2.1 编译

```bash
cd go-proxy-cto
go build -o cto-proxy .
```

### 2.2 运行

```bash
# 以 accounts.jsonl 所在目录为 baseDir 启动
cd cto.new注册反代
./go-proxy-cto/cto-proxy
# 或者后台运行
CTO_BASE_DIR="$(pwd)" nohup ./go-proxy-cto/cto-proxy > /tmp/cto-proxy.log 2>&1 &
disown
```

### 2.2.1 自定义 API Key

默认外部访问需要 `Authorization: Bearer xs2-cto-secret`（只是个约定值，不是真密码）。通过环境变量自定义：

```bash
export PROXY_API_KEY="your-own-secret"
./go-proxy-cto/cto-proxy
```

听地址默认 `127.0.0.1:9091`，可改 `config.go` 中的 `ListenAddr`。

访问 `http://127.0.0.1:9091/` 打开 Dashboard，`http://127.0.0.1:9091/status` 看 JSON 统计。

### 2.3 调用示例

**OpenAI 兼容**：

```bash
curl -N http://127.0.0.1:9091/v1/chat/completions \
  -H 'Authorization: Bearer any' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.4",
    "stream": true,
    "messages": [{"role": "user", "content": "hi"}]
  }'
```

**Anthropic 兼容**：

```bash
curl -N http://127.0.0.1:9091/v1/messages \
  -H 'x-api-key: any' \
  -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "glm-5.1",
    "stream": true,
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "hi"}]
  }'
```

> 任意 Authorization 头都能通过（保留字段，未来做多租户时用），**反代从 accounts.jsonl 里选一个可用账号来完成真正调用**。

### 2.4 支持的模型

通过 `/v1/models` 可获取完整列表。核心映射：

| 请求名 | → engineKey | 实际模型 | 图像 | 每次计费 | 免费 |
|---|---|---|---|---|---|
| `gpt-5.4` | `gpt_5_4` | GPT 5.4 | ✅ | ×3 | ✅ |
| `glm-5.1` | `glm_5_1` | GLM 5.1 | ❌ | ×2 | ✅ |
| `gpt-4o` / `gpt-4` / `o1` / `claude-sonnet-4.6` / … | `gpt_5_4` | GPT 5.4（别名） | ✅ | ×3 | ✅ |

每个 Free 账号每日 **300** / 每周 **900** 配额（按 `usageMultiplier` 扣）。

### 2.5 管理端点

| 路径 | 说明 |
|---|---|
| `GET /status` | 账号池统计 |
| `GET /v1/models` | 支持的模型列表 |
| `GET /admin/accounts` | 所有账号的状态（email, in_use, exhausted, dead, success, errors） |
| `POST /admin/reload` | 手动热加载 accounts.jsonl |

## 三、号池分发

反代启动时：

1. **读取** `accounts.jsonl` → 构造 Account 列表
2. **预热**：所有账号并行刷新 JWT（`POST /v1/client/sessions/{sid}/tokens` + `__client` cookie）
3. 每 30 秒扫一次 `accounts.jsonl` 热加载新注册的账号

请求到达时：

1. **LRU 取一个 `IsAvailable()` 的账号**（非 in_use / 非 exhausted / 非 dead）
2. 确保其 JWT 剩余 > 50 秒，否则刷新
3. 走 `/projects/create-hosted` → `/engine-agent/offers` → `/engine-agent/chat` → WebSocket 收流
4. 结束后归还，若返回文本里有配额关键词（limit/quota/exceeded/…）→ 标 `exhausted`（6h 冷却）
5. 若 Clerk 401 → 标 `dead`（当前轮次不再使用）

**当所有账号 exhausted/dead** 时，反代返回：

```json
{"error": "No available accounts (all exhausted or dead)"}
```

→ 此时跑 `cto_register_pw.py --start N --count M` 补充账号；若只是 cookie 过期，用 `cto_login_refresh.py` 更快。

## 四、Cookie 刷新器（Turnstile 救命稻草）

反代跑久了（或重启多次）会让 `__client` cookie 轮换失效，体现为 Clerk refresh 返回 401。此时：

```bash
# 刷所有账号（有效的跳过，失效的重新登录）
python3 cto_login_refresh.py

# 只刷某一个
python3 cto_login_refresh.py --email 1@your-domain.email --show
```

**关键：登录页 `/sign-in` 没有 Turnstile**（只注册页有），所以这个脚本在 IP 被风控时依然能用。全程：

1. 打开 sign-in 页
2. 填 email / password（从 `accounts.jsonl`)
3. 提交 → 等跳转回 `cto.new/`
4. `context.new_cdp_session(page).send("Network.getAllCookies")` 抓所有 cookie
5. 提取 `__client` / `__session` / `__client_uat` / `clerk_session_id`
6. `verify_refresh()` 自检能刷 JWT
7. 覆盖 `accounts.jsonl` 对应行

## 五、已验证（最新实测）

| 测试 | 结果 |
|---|---|
| OpenAI `/v1/chat/completions` 非流式 GPT-5.4 | ✅ `{"content":"42"}` 正确返回 |
| OpenAI `/v1/chat/completions` 流式 GPT-5.4 | ✅ 6 个 delta chunk（`1`/` `/`2`/` `/`3`）+ `[DONE]` |
| Anthropic `/v1/messages` 流式 GLM-5.1 | ✅ 完整 SSE：`message_start → content_block_delta × N → message_stop`，回复 `Hi! 👋` |
| Clerk JWT 自动刷新（`__client` cookie） | ✅ `POST /v1/client/sessions/{sid}/tokens` → `status=200, jwt_len=889` |
| Cookie 过期自动恢复 | ✅ `cto_login_refresh.py` 登录后 1.4s 表单出现，回抓 JWT 刷新成功 |
| 图像识别（GPT-5.4 多模态） | ✅ 浏览器实测："Red circle, CTO-TEST" 正确识别 |
| WebSocket token 级流式 | ✅ 接收 `type:update` 帧，`chat.content` 拼接正常 |

## 六、局限与 TODO

- **Turnstile 风控**：同 IP 短时间多次自动化注册必被拦。建议配代理（`launch(proxy={server: "socks5://..."})` 或 ssh tunnel）或按小时分批。
- **图像上传**待集成到 `/v1/chat/completions`：协议已抓 `POST /images/presign` → S3 PUT → `imageUrls` 数组，Go 侧保留了 `connectAndStream(..., imageURLs)` 参数位但 handler 里未接入。
- **Anthropic tools / 多轮 `tool_use`** 未实现（cto.new 的 agent 用的 tool 协议和 Anthropic 不兼容；要实现需改 `handler_anthropic.go` 做三阶段回放）。
- **配额统计** 目前只能靠响应文本关键词（limit/quota/exceeded）标 exhausted；真正精确数字需要周期调 `/billing`，未做。

详细协议、字段、Turnstile 诊断过程和每个踩坑见 [TECH.md](TECH.md)。
