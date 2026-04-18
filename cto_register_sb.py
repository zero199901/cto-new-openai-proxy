#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
cto.new 批量注册机 —— SeleniumBase UC Mode 版（主力）
=====================================================
为什么选 SeleniumBase UC Mode：
* 基于 undetected-chromedriver，指纹被 Cloudflare Turnstile 识别率远低于原生
  Selenium / Playwright
* 自带 `uc_gui_click_captcha()`：操作系统级 PyAutoGUI 点击，能直接过 Turnstile
  可交互挑战（Clerk 升级到 visible challenge 时）
* `uc_open_with_reconnect()`：打开页面后短暂断开 WebDriver 连接，避开 CF 的
  headless / automation 检测帧
* 出问题时可退到 `bypass_cloudflare()` 先拿 `cf_clearance` 再注入浏览器

邮箱服务统一由 mail_client.MailClient 提供（默认 GPTMail，
https://mail.chatgpt.org.uk，每日公共额度 20 万）。

用法：
  python3 cto_register_sb.py --count 5                # 批量注册 5 个
  python3 cto_register_sb.py --count 0                # 无限循环注册（直到手动 Ctrl+C 或连续失败）
  python3 cto_register_sb.py --count 1 --headless     # 无头模式（不推荐）
  python3 cto_register_sb.py --count 1 --proxy http://127.0.0.1:7890

产物 accounts.jsonl 字段和 cto_register_pw.py 完全一致，反代直接复用。
"""
from __future__ import annotations
import os
import sys
import re
import json
import time
import random
import string
import logging
import argparse
import requests
from datetime import datetime
from pathlib import Path
from typing import Optional

from seleniumbase import SB

from api_mail_client import APIMailClient

BASE_DIR = Path(__file__).resolve().parent

logging.basicConfig(
    level=logging.INFO,
    format="[%(asctime)s] %(levelname)s - %(message)s",
    datefmt="%H:%M:%S",
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler(BASE_DIR / "cto_register_sb.log", encoding="utf-8"),
    ],
)
log = logging.getLogger(__name__)

# ============================================================
# 常量
# ============================================================
SIGNUP_URL = "https://accounts.cto.new/sign-up"
AFTER_SIGNUP_URL = "https://cto.new/"
OUTPUT_FILE = BASE_DIR / "accounts.jsonl"

FORM_WAIT_SEC = 15         # Clerk 表单渲染最长等待
VERIFY_URL_TIMEOUT = 30    # 跳转 verify 页超时（一般 3-8s）
MAIL_TIMEOUT = 75          # 邮件验证码等待总时长
AFTER_VERIFY_TIMEOUT = 45  # 验证后跳回 cto.new/ 超时
CLERK_SESSION_TIMEOUT = 15 # Clerk session 注入等待
JWT_TIMEOUT = 15           # JWT 获取超时
DEBUG = False              # 调试模式（--debug 开启）

UA_CHROME = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.0.0 Safari/537.36"
)


# ============================================================
# 工具
# ============================================================
def random_password() -> str:
    """Clerk 要求 8+ 位，含大小写+数字+符号。"""
    core = "".join(random.choices(string.ascii_letters + string.digits, k=12))
    return f"CtoProxy!{core}"


def verify_refresh(client_cookie: str, session_id: str) -> bool:
    """用 __client cookie 调一次 Clerk refresh，验证反代可用。"""
    if not client_cookie or not session_id:
        return False
    try:
        r = requests.post(
            f"https://clerk.cto.new/v1/client/sessions/{session_id}/tokens"
            f"?__clerk_api_version=2025-11-10&_clerk_js_version=6.7.3",
            headers={
                "Origin": "https://cto.new",
                "Referer": "https://cto.new/",
                "User-Agent": UA_CHROME,
                "Cookie": f"__client={client_cookie}",
            },
            timeout=10,
        )
        return r.status_code == 200 and len(r.json().get("jwt", "")) > 100
    except Exception as e:
        log.warning(f"verify_refresh 异常: {e}")
        return False


# ============================================================
# 反爬 / Cloudflare 检测辅助
# ============================================================
CF_INDICATORS = [
    "challenges.cloudflare.com",
    "just a moment",
    "verify you are human",
    "cf-browser-verification",
    "checking your browser",
]


def detect_cloudflare_challenge(sb) -> bool:
    """判断当前页是否卡在 Cloudflare 挑战上（区别于 Clerk 内嵌的 invisible Turnstile）。

    * Clerk 页 = domain 是 accounts.cto.new，即便嵌了 Turnstile iframe 也不算被拦
    * CF 挑战页 = body 很小 + challenge iframe + 关键词命中
    """
    try:
        url = sb.get_current_url()
        if "challenges.cloudflare.com" in url:
            return True
        # iframe 存在且是 top-level（非 Clerk invisible 挂件）
        # UC Mode 下 execute_script 用 CDP evaluate，只接受表达式，用 IIFE 返回值
        has_top_cf_iframe = sb.execute_script("""
            (function(){
                const ifs = document.querySelectorAll("iframe[src*='challenges.cloudflare.com']");
                if (!ifs.length) return false;
                for (const f of ifs) {
                    const r = f.getBoundingClientRect();
                    if (r.width > 200 && r.height > 50) return true;
                }
                return false;
            })()
        """)
        if has_top_cf_iframe:
            return True
        src = sb.get_page_source().lower()
        hits = sum(1 for x in CF_INDICATORS if x in src)
        return hits >= 2
    except Exception:
        return False


def try_bypass_cloudflare(sb) -> bool:
    """如果检测到 CF 挑战，尝试用 SB 的 uc_gui_click_captcha 过掉。"""
    for attempt in range(3):
        log.info(f"  CF 挑战绕过尝试 #{attempt + 1}")
        try:
            sb.uc_gui_click_captcha()
        except Exception as e:
            log.warning(f"    uc_gui_click_captcha 失败: {e}")
        sb.sleep(4)
        if not detect_cloudflare_challenge(sb):
            log.info("  ✅ CF 挑战已通过")
            return True
    log.error("  ❌ CF 挑战多次尝试未过")
    return False



# ============================================================
# CDP main world 执行辅助
# ============================================================
def cdp_eval(sb, expression: str, return_by_value: bool = True):
    """在 main world 执行 JS 表达式（绕过 UC Mode isolated world 限制）。

    UC Mode 下 sb.execute_script 走 CDP Runtime.evaluate 的 isolated world，
    无法访问页面 main world 的 window.Clerk 等对象。
    此函数用 CDP Runtime.evaluate 的默认 execution context（main world）执行。
    """
    try:
        result = sb.driver.execute_cdp_cmd("Runtime.evaluate", {
            "expression": expression,
            "returnByValue": return_by_value,
            "awaitPromise": False,
        })
        if result and "result" in result:
            r = result["result"]
            if r.get("type") == "object" and r.get("subtype") == "error":
                log.debug(f"cdp_eval error: {r.get('description', '')}")
                return None
            return r.get("value")
        return None
    except Exception as e:
        log.debug(f"cdp_eval 异常: {e}")
        return None


# ============================================================
# 单个账号注册
# ============================================================
def register_one(sb, mail: APIMailClient) -> dict:
    """纯 API 邮箱 + 浏览器注册流程。

    APIMailClient 用 requests 直接调 GPTMail REST API，不需要浏览器 tab，
    彻底消除 mail tab 广告弹窗劫持焦点的问题。

    流程耗时分析（优化后）：
      API 生成邮箱 ~1.5s + 页面加载 ~5s + 填表+提交 ~3s +
      等 verify+验证码 ~5s + OTP 填入 ~1s + 等跳转 ~15s +
      Clerk session+JWT+cookie ~3s ≈ 33s
    """
    t0 = time.time()
    password = random_password()

    # ---- STEP 1: API 生成临时邮箱 ----
    try:
        email = mail.generate()
    except Exception as e:
        log.error(f"  ❌ API 生成邮箱失败: {e}")
        return {}
    known_ids = mail.list_ids(email)
    log.info(f"  [{time.time()-t0:.1f}s] 邮箱: {email}  密码: {password}")

    # ---- STEP 2: 打开 signup 页面 ----
    try:
        sb.uc_open_with_reconnect(SIGNUP_URL, reconnect_time=3)
    except Exception as e:
        log.warning(f"  uc_open_with_reconnect 失败，退回 open: {e}")
        sb.open(SIGNUP_URL)

    if detect_cloudflare_challenge(sb):
        log.info("  检测到顶层 Cloudflare 挑战")
        if not try_bypass_cloudflare(sb):
            return {}

    # ---- STEP 3: 等 Clerk 表单渲染 + 填表 ----
    try:
        sb.wait_for_element_visible("input#emailAddress-field", timeout=FORM_WAIT_SEC)
    except Exception:
        log.error(f"  ❌ 表单渲染超时, URL: {sb.get_current_url()}")
        return {}
    log.info(f"  [{time.time()-t0:.1f}s] 表单 ready，填入邮箱+密码+勾选协议")

    # React controlled input 必须用 native setter 触发 React 事件
    sb.execute_script(f"""
        (function(){{
            function setNV(el, v) {{
                const s = Object.getOwnPropertyDescriptor(
                    Object.getPrototypeOf(el), 'value'
                ).set;
                s.call(el, v);
                el.dispatchEvent(new Event('input', {{bubbles: true}}));
                el.dispatchEvent(new Event('change', {{bubbles: true}}));
            }}
            const e = document.getElementById('emailAddress-field');
            const p = document.getElementById('password-field');
            const t = document.getElementById('legalAccepted-field');
            if (e) {{ e.focus(); setNV(e, {json.dumps(email)}); }}
            if (p) {{ p.focus(); setNV(p, {json.dumps(password)}); }}
            if (t && !t.checked) t.click();
        }})()
    """)
    sb.sleep(0.3)

    # ---- STEP 4: 提交 ----
    # Clerk Turnstile smart 模式：invisible 通道自动处理 token，
    # 但需要给 Turnstile 一点时间完成。短等 2s token 就绪信号，
    # 如果 2s 内 token 就绪则立即提交；否则直接提交让 Clerk 决定。
    token_wait = 2.0
    token_deadline = time.time() + token_wait
    while time.time() < token_deadline:
        # 检测 token 或 submit 按钮 enabled
        token_ok = cdp_eval(sb, """
            (function(){
                const names = ['cf-turnstile-response', 'clerk-captcha-token'];
                for (const n of names) {
                    const el = document.querySelector("input[name='" + n + "']");
                    if (el && el.value && el.value.length > 10) return true;
                }
                const btns = document.querySelectorAll('button[type="submit"]');
                for (const b of btns) {
                    if (!b.disabled && b.getBoundingClientRect().width > 0) return true;
                }
                return false;
            })()
        """)
        if token_ok:
            break
        sb.sleep(0.3)

    log.info(f"  [{time.time()-t0:.1f}s] 点击 Continue")
    for sel in ['button[type="submit"]', '.cl-formButtonPrimary']:
        try:
            if sb.is_element_visible(sel):
                try:
                    sb.uc_click(sel, reconnect_time=3)
                except AttributeError:
                    sb.click(sel)
                except Exception:
                    sb.js_click(sel)
                break
        except Exception:
            continue
    else:
        try:
            sb.send_keys('input#password-field', '\ue007')
        except Exception:
            pass

    # ---- STEP 5: 统一轮询等 URL 跳转（合并 verify 检测 + captcha 兜底）----
    # 点击后快速轮询 URL：5s 内跳到 verify 就跳过 captcha 兜底
    # 超过 5s 未跳转才触发 uc_gui_click_captcha（visible Turnstile 场景）
    click_time = time.time()
    log.info(f"  [{time.time()-t0:.1f}s] 等 URL 跳转 verify...")
    verify_ok = False
    captcha_tried = False
    deadline = time.time() + VERIFY_URL_TIMEOUT
    while time.time() < deadline:
        try:
            cur = sb.get_current_url()
        except Exception:
            sb.sleep(0.3)
            continue
        if "verify" in cur.lower():
            verify_ok = True
            break
        # 点击后 5s 还没跳转 → 尝试一次 captcha 兜底
        if not captcha_tried and time.time() - click_time > 5:
            captcha_tried = True
            log.info("  URL 未跳转 verify → uc_gui_click_captcha 兜底")
            try:
                sb.uc_gui_click_captcha(retry=True, blind=False)
            except Exception:
                pass
        sb.sleep(0.3)

    if not verify_ok:
        log.error(f"  ❌ 跳转 verify 超时, URL: {sb.get_current_url()}")
        _dump_error_page(sb, "verify_timeout")
        return {}
    log.info(f"  [{time.time()-t0:.1f}s] ✅ 进入 verify 页")

    # ---- STEP 6: 等验证码 + 填 OTP ----
    try:
        code = mail.wait_code(email, known_ids, timeout=MAIL_TIMEOUT)
    except TimeoutError as e:
        log.error(f"  ❌ 邮件验证码等待超时: {e}")
        return {}
    log.info(f"  [{time.time()-t0:.1f}s] 收到验证码: {code}")

    # 等 OTP 输入框渲染 + 填入（cdp_eval main world）
    otp_result = _fill_otp(sb, code)
    if not otp_result:
        log.warning("  OTP 填入失败，尝试兜底 submit")
    log.info(f"  [{time.time()-t0:.1f}s] OTP 填入: {otp_result}")

    # 兜底 submit（Clerk 单框 OTP 可能不会自动 submit）
    try:
        if sb.is_element_visible('button[type="submit"]'):
            sb.click('button[type="submit"]')
    except Exception:
        pass

    # ---- STEP 7: 等跳转回 cto.new/ ----
    land_ok = False
    deadline = time.time() + AFTER_VERIFY_TIMEOUT
    while time.time() < deadline:
        if re.match(r"^https://cto\.new/", sb.get_current_url()):
            land_ok = True
            break
        sb.sleep(0.3)
    if not land_ok:
        log.error(f"  ❌ 验证后跳转超时, URL: {sb.get_current_url()}")
        return {}
    log.info(f"  [{time.time()-t0:.1f}s] ✅ 已跳转到 cto.new/")

    # ---- STEP 8: 等 Clerk session + 拿信息 + JWT ----
    clerk_sync = _wait_and_get_clerk_info(sb, t0)
    if not clerk_sync:
        return {}
    jwt = _get_clerk_jwt(sb, t0)
    if not jwt:
        return {}

    # ---- STEP 9: 用 JWT 拿 engine user/workspaces ----
    cu_raw, workspaces = _fetch_engine_info(jwt)

    # ---- STEP 10: 取 cookie（CDP getAllCookies 一步到位）----
    ck_client, ck_session, ck_client_uat = _get_clerk_cookies(sb)
    if not ck_client:
        log.error("  ❌ 没拿到 __client cookie")
        return {}

    session_id = clerk_sync.get("clerk_session_id", "")
    if not verify_refresh(ck_client, session_id):
        log.error("  ❌ __client cookie 自检失败（Clerk refresh 401）")
        return {}

    # ---- STEP 11: 组装账号数据 ----
    cu = (cu_raw.get("currentUser") if cu_raw else None) or {}
    team = cu.get("team", {}) or {}
    ws_list = workspaces or []
    default_ws = ws_list[0] if ws_list else {}

    acc = {
        "email": email,
        "password": password,
        "clerk_user_id": clerk_sync.get("clerk_user_id", ""),
        "clerk_session_id": session_id,
        "clerk_org_id": clerk_sync.get("clerk_org_id", ""),
        "clerk_client_cookie": ck_client,
        "clerk_session_cookie": ck_session or jwt,
        "clerk_client_uat": ck_client_uat,
        "engine_user_id": cu.get("id", ""),
        "engine_team_id": team.get("id", ""),
        "engine_api_key": team.get("apiKey", ""),
        "default_workspace_id": default_ws.get("id", ""),
        "registered_at": datetime.now().isoformat(timespec="seconds"),
        "ws_token_suffix": f"{clerk_sync.get('clerk_user_id','')}:{clerk_sync.get('clerk_org_id','')}",
    }
    log.info(
        f"  ✅ 注册+自检通过 [{time.time()-t0:.1f}s] "
        f"team={acc['engine_team_id'][:8]}... client={ck_client[:18]}..."
    )
    return acc


# ============================================================
# register_one 的子步骤
# ============================================================
def _fill_otp(sb, code: str) -> str:
    """等 OTP 输入框渲染 + 填入验证码。返回填入方式描述。"""
    # 等 OTP 输入框（最多 6s）
    for _ in range(20):
        has = cdp_eval(sb, """
            !!document.querySelector(
                'input[autocomplete="one-time-code"], input[inputmode="numeric"], ' +
                'input[name*="code"], input[name*="otp"], input[maxlength="1"]'
            )
        """)
        if has:
            break
        sb.sleep(0.3)

    # cdp_eval 在 main world 填入 OTP
    result = cdp_eval(sb, f"""
        (function(){{
            const code = {json.dumps(code)};
            function setNV(el, v) {{
                const s = Object.getOwnPropertyDescriptor(
                    Object.getPrototypeOf(el), 'value'
                ).set;
                s.call(el, v);
                el.dispatchEvent(new Event('input', {{bubbles: true}}));
                el.dispatchEvent(new Event('change', {{bubbles: true}}));
            }}
            const single = document.querySelector('input[autocomplete="one-time-code"]');
            if (single && single.maxLength !== 1) {{
                single.focus(); setNV(single, code); return 'single-otp';
            }}
            const numeric = document.querySelector('input[inputmode="numeric"]');
            if (numeric && numeric.maxLength !== 1) {{
                numeric.focus(); setNV(numeric, code); return 'numeric';
            }}
            const boxes = document.querySelectorAll('input[maxlength="1"]');
            if (boxes.length >= code.length) {{
                for (let i = 0; i < code.length; i++) {{
                    boxes[i].focus(); setNV(boxes[i], code[i]);
                }}
                return 'boxes(' + boxes.length + ')';
            }}
            return 'no-input-found';
        }})()
    """)
    if result and result != 'no-input-found':
        return result

    # cdp_eval 失败 → selenium 原生方法退路
    from selenium.webdriver.common.by import By
    from selenium.webdriver.support.ui import WebDriverWait
    from selenium.webdriver.support import expected_conditions as EC
    try:
        wait = WebDriverWait(sb.driver, 5)
        el = wait.until(EC.presence_of_element_located((
            By.CSS_SELECTOR,
            'input[autocomplete="one-time-code"], input[inputmode="numeric"], '
            'input[name*="code"], input[maxlength="1"]'
        )))
        el.click()
        sb.sleep(0.2)
        el.send_keys(code)
        return 'selenium-send_keys'
    except Exception:
        return ''


def _wait_and_get_clerk_info(sb, t0: float) -> dict:
    """等 Clerk session 注入 + 拿同步信息。"""
    deadline = time.time() + CLERK_SESSION_TIMEOUT
    while time.time() < deadline:
        if cdp_eval(sb, "!!(window.Clerk && window.Clerk.session && window.Clerk.user)"):
            break
        sb.sleep(0.3)

    clerk_raw = cdp_eval(sb, """
        (function(){
            try {
                const sess = window.Clerk && window.Clerk.session;
                const user = window.Clerk && window.Clerk.user;
                if (!sess || !user) return JSON.stringify({error: 'clerk_not_ready'});
                return JSON.stringify({
                    clerk_user_id: user.id,
                    clerk_session_id: sess.id,
                    clerk_org_id: sess.lastActiveOrganizationId || '',
                    email: (user.emailAddresses && user.emailAddresses[0])
                           ? user.emailAddresses[0].emailAddress : '',
                });
            } catch (e) {
                return JSON.stringify({error: String(e)});
            }
        })()
    """)
    if not clerk_raw:
        log.error(f"  ❌ Clerk 同步信息为空 [{time.time()-t0:.1f}s]")
        return {}
    try:
        info = json.loads(clerk_raw)
    except Exception:
        log.error(f"  ❌ Clerk JSON 解析失败: {clerk_raw[:100]}")
        return {}
    if info.get("error"):
        log.error(f"  ❌ Clerk 未就绪: {info['error']} [{time.time()-t0:.1f}s]")
        return {}
    log.info(f"  [{time.time()-t0:.1f}s] Clerk user={info['clerk_user_id']}")
    return info


def _get_clerk_jwt(sb, t0: float) -> str:
    """异步拿 Clerk JWT（getToken 是 Promise）。"""
    cdp_eval(sb, """
        (function(){
            window.__jwt = null;
            window.__jwt_err = null;
            (async () => {
                try {
                    const tok = await window.Clerk.session.getToken();
                    window.__jwt = tok || '';
                } catch (e) {
                    window.__jwt_err = String(e);
                }
            })();
        })()
    """, return_by_value=False)
    deadline = time.time() + JWT_TIMEOUT
    while time.time() < deadline:
        tok = cdp_eval(sb, "window.__jwt")
        err = cdp_eval(sb, "window.__jwt_err")
        if tok:
            log.info(f"  [{time.time()-t0:.1f}s] JWT len={len(tok)}")
            return tok
        if err:
            log.error(f"  ❌ getToken 报错: {err}")
            return ""
        sb.sleep(0.3)
    log.error(f"  ❌ JWT 超时 [{time.time()-t0:.1f}s]")
    return ""


def _fetch_engine_info(jwt: str):
    """用 Python requests 拿 engine user/workspaces。"""
    headers = {
        "Authorization": f"Bearer {jwt}",
        "User-Agent": UA_CHROME,
        "Origin": "https://cto.new",
        "Referer": "https://cto.new/",
    }
    cu = {}
    workspaces = []
    try:
        r = requests.get("https://api.enginelabs.ai/current-user",
                         headers=headers, timeout=15)
        if r.status_code == 200:
            cu = r.json()
    except Exception as e:
        log.warning(f"    current-user 异常: {e}")
    try:
        r = requests.get("https://api.enginelabs.ai/workspaces?pageSize=100",
                         headers=headers, timeout=15)
        if r.status_code == 200:
            workspaces = r.json().get("workspaces", [])
    except Exception as e:
        log.warning(f"    workspaces 异常: {e}")
    return cu, workspaces


def _get_clerk_cookies(sb) -> tuple:
    """获取 Clerk cookie（__client / __session / __client_uat）。

    优先用 CDP Network.getAllCookies（能拿到所有域名包括 .clerk.cto.new 的 cookie），
    selenium get_cookies 只返回当前域名。
    """
    ck_client = ck_session = ck_client_uat = ""

    # CDP getAllCookies 一步到位
    try:
        all_cookies = sb.driver.execute_cdp_cmd("Network.getAllCookies", {})
        for c in all_cookies.get("cookies", []):
            name = c.get("name", "")
            dom = c.get("domain", "")
            if "cto.new" not in dom and "clerk" not in dom:
                continue
            if name == "__client" and not ck_client:
                ck_client = c.get("value", "")
            elif name == "__session" and not ck_session:
                ck_session = c.get("value", "")
            elif name == "__client_uat" and not ck_client_uat:
                ck_client_uat = c.get("value", "")
    except Exception as e:
        log.warning(f"  CDP getAllCookies 失败: {e}")

    # CDP 失败时退到 selenium get_cookies
    if not ck_client:
        for c in sb.get_cookies():
            name = c.get("name")
            dom = c.get("domain", "")
            if "cto.new" not in dom and "clerk" not in dom:
                continue
            if name == "__client" and not ck_client:
                ck_client = c["value"]
            elif name == "__session" and not ck_session:
                ck_session = c["value"]
            elif name == "__client_uat" and not ck_client_uat:
                ck_client_uat = c["value"]

    return ck_client, ck_session, ck_client_uat


def _dump_error_page(sb, tag: str):
    """调试模式：dump 当前页面 HTML。"""
    if not DEBUG:
        return
    try:
        dbg = BASE_DIR / f"debug_sb_{tag}.html"
        dbg.write_text(sb.get_page_source(), encoding="utf-8")
        log.info(f"  [debug] 页面已 dump 到 {dbg}")
    except Exception:
        pass


def save_account(acc: dict, path: Path):
    with open(path, "a", encoding="utf-8") as f:
        f.write(json.dumps(acc, ensure_ascii=False) + "\n")
    log.info(f"写入 {path.name}: {acc['email']}")


# ============================================================
# 主入口
# ============================================================
def main():
    ap = argparse.ArgumentParser(description="cto.new 批量注册（SeleniumBase UC Mode 版）")
    ap.add_argument("--count", type=int, default=1,
                    help="要注册的账号数量，0 表示无限循环")
    ap.add_argument("--output", type=str, default=str(OUTPUT_FILE))
    ap.add_argument("--headless", action="store_true",
                    help="无头模式（不推荐，CF 检测率显著上升）")
    ap.add_argument("--proxy", type=str, default=None,
                    help="代理 http://host:port 或 socks5://host:port")
    ap.add_argument("--gap-min", type=float, default=4.0,
                    help="账号之间最小间隔秒数")
    ap.add_argument("--gap-max", type=float, default=10.0,
                    help="账号之间最大间隔秒数")
    ap.add_argument("--max-fail", type=int, default=5,
                    help="连续失败 N 次后停止（0 表示永不停止）")
    ap.add_argument("--debug", action="store_true",
                    help="调试模式：dump 页面 HTML、输出详细 DOM 信息")
    args = ap.parse_args()

    global DEBUG
    DEBUG = args.debug

    out = Path(args.output)
    log.info("=" * 60)
    log.info(f"cto.new 批量注册机 (SeleniumBase UC Mode)")
    log.info(f"  数量: {args.count}  代理: {args.proxy or '无'}  邮箱: mail.chatgpt.org.uk")
    log.info(f"  输出: {out}")
    log.info("=" * 60)

    infinite = args.count == 0
    ok = fail = consec_fail = 0
    i = 0
    t_start = time.time()

    # SeleniumBase UC 必须用 SB context manager，每个账号一个新 SB 实例
    try:
        while True:
            i += 1
            label = f"#{i}" if infinite else f"#{i}/{args.count}"
            log.info("")
            log.info(f">>>>>>>>>>>>>>>> 账号 {label} <<<<<<<<<<<<<<<<")
            try:
                with SB(
                    uc=True,
                    test=True,
                    locale="en",
                    headless=args.headless,
                    proxy=args.proxy,
                    agent=UA_CHROME,
                ) as sb:
                    mail = APIMailClient()
                    acc = register_one(sb, mail)
                    if acc and acc.get("engine_team_id"):
                        save_account(acc, out)
                        ok += 1
                        consec_fail = 0
                    else:
                        fail += 1
                        consec_fail += 1
                        log.warning(f"  {label} 失败 (连续失败 {consec_fail} 次)")
            except KeyboardInterrupt:
                raise
            except Exception as e:
                fail += 1
                consec_fail += 1
                log.error(f"  {label} 异常: {e} (连续失败 {consec_fail} 次)")

            # 连续失败保护
            if args.max_fail > 0 and consec_fail >= args.max_fail:
                log.error(f"连续失败 {consec_fail} 次，达到 --max-fail 阈值，停止注册")
                break

            # 非无限模式：到达指定数量后停止
            if not infinite and i >= args.count:
                break

            # 间隔休息
            delay = random.uniform(args.gap_min, args.gap_max)
            elapsed = time.time() - t_start
            rate = ok / (elapsed / 60) if elapsed > 60 else 0
            log.info(
                f"  休息 {delay:.1f}s ... | "
                f"累计: 成功 {ok} / 失败 {fail} | "
                f"速率: {rate:.1f} 个/分钟"
            )
            time.sleep(delay)
    except KeyboardInterrupt:
        log.info("\n收到 Ctrl+C，停止注册")

    elapsed = time.time() - t_start
    log.info("=" * 60)
    log.info(f"注册结束! 成功 {ok} / 失败 {fail} / 总耗时 {elapsed/60:.1f} 分钟")
    if ok > 0:
        log.info(f"平均速率: {ok / (elapsed / 60):.2f} 个/分钟")
    log.info(f"输出文件: {out}")
    log.info("=" * 60)


if __name__ == "__main__":
    main()
