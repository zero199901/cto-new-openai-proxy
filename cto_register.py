#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
cto.new 批量注册机（参照 anything_register.py 改造）
=====================================================
流程：
  1. 生成 {idx}@<your-domain>（邮件真实投递到 <your-subdomain>@mailto.plus）
  2. Selenium 打开 https://accounts.cto.new/sign-up
  3. 填邮箱 + 随机强密码 + 勾选 TOS，点 Continue
  4. Clerk 的 invisible Turnstile 在浏览器环境自动通过
  5. 跳转到 verify-email-address 页面
  6. 从 tempmail.plus 轮询主题为 "{code} is your verification code" 的邮件
  7. 用正则抠 6 位数字填入，提交
  8. 自动跳到 https://cto.new/，从 window.Clerk 提取 session JWT + cookie
  9. 调 https://api.enginelabs.ai/current-user 拿 engine_team_id + engine_api_key
 10. 追加写入 accounts.jsonl（给 go-proxy-cto 热加载）

输出格式（accounts.jsonl 每行一个 JSON）：
  {
    "email": "1@<your-domain>",
    "password": "CtoProxy!xYzAbc9qPl",
    "clerk_user_id": "user_xxx",
    "clerk_session_id": "sess_xxx",
    "clerk_org_id": "org_xxx",
    "clerk_session_cookie": "<__session JWT 值>",
    "clerk_client_uat": "1776411128",
    "engine_user_id": "<uuid>",
    "engine_team_id": "<uuid>",
    "engine_api_key": "<固定 key>",
    "default_workspace_id": "<uuid>",
    "registered_at": "2026-04-17T07:32:09"
  }

用法：
  python3 cto_register.py --start 1 --count 5
  python3 cto_register.py --start 1 --count 1 --no-headless   # 看浏览器
"""

# ============================================================
# 导入依赖
# ============================================================
import os
import sys
import re
import gc
import json
import time
import random
import string
import logging
import argparse
import tempfile
import shutil
from pathlib import Path
from datetime import datetime

import requests
from selenium import webdriver
from selenium.webdriver.common.by import By
from selenium.webdriver.chrome.service import Service as ChromeService
from selenium.webdriver.chrome.options import Options
from selenium.webdriver.support.ui import WebDriverWait
from selenium.webdriver.support import expected_conditions as EC
from selenium.common.exceptions import TimeoutException, WebDriverException

# 抑制第三方库噪声
logging.getLogger("selenium").setLevel(logging.ERROR)
logging.getLogger("urllib3").setLevel(logging.ERROR)
logging.getLogger("WDM").setLevel(logging.ERROR)

# ============================================================
# 项目根目录
# ============================================================
BASE_DIR = Path(__file__).resolve().parent

# ============================================================
# 日志
# ============================================================
logging.basicConfig(
    level=logging.INFO,
    format="[%(asctime)s] %(levelname)s - %(message)s",
    datefmt="%H:%M:%S",
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler(BASE_DIR / "cto_register.log", encoding="utf-8"),
    ],
)
log = logging.getLogger(__name__)

# ============================================================
# 常量
# ============================================================
SIGNUP_URL = "https://accounts.cto.new/sign-up"
AFTER_SIGNUP_URL = "https://cto.new/"
API_BASE = "https://api.enginelabs.ai"

# ⬇️ 使用前请修改这两个常量为你自己的 tempmail.plus 子域
# 或通过环境变量 CTO_EMAIL_DOMAIN / CTO_TEMPMAIL_RECEIVER 覆盖
EMAIL_DOMAIN = os.environ.get("CTO_EMAIL_DOMAIN", "example.email")
TEMPMAIL_RECEIVER = os.environ.get("CTO_TEMPMAIL_RECEIVER", "example@mailto.plus")
TEMPMAIL_API = "https://tempmail.plus/api"

OUTPUT_FILE = BASE_DIR / "accounts.jsonl"
PROGRESS_FILE = BASE_DIR / "cto_register_progress.json"

PAGE_LOAD_TIMEOUT = 60
WAIT_TIMEOUT = 60  # Cloudflare JS challenge 通常 15-25s，留足够缓冲
MAIL_TIMEOUT = 120
MAIL_INTERVAL = 3
VERIFY_TIMEOUT = 60

# 匹配 Clerk 验证码邮件：主题或正文里 6 位数字
CODE_SUBJECT_RE = re.compile(r"^\s*(\d{6})\s+is your verification code", re.IGNORECASE)
CODE_BODY_RE = re.compile(r"\b(\d{6})\b")

# Clerk 永远用这些查询参数；写死版本避免每次抓
CLERK_API_VERSION = "2025-11-10"
CLERK_JS_VERSION_FALLBACK = "6.7.3"


# ============================================================
# tempmail.plus 邮件客户端（对齐 anything_register.py）
# ============================================================
class TempMailPlus:
    """tempmail.plus 读邮箱。所有邮件都投递到同一个 receiver（如 <your-subdomain>@mailto.plus），
    通过邮件内部的 To 字段区分具体哪个子地址。"""

    def __init__(self, receiver: str):
        self.receiver = receiver
        self.s = requests.Session()
        self.s.headers.update({
            "User-Agent": (
                "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
                "AppleWebKit/537.36 (KHTML, like Gecko) "
                "Chrome/147.0.0.0 Safari/537.36"
            ),
            "Accept": "application/json",
        })

    def get_mails(self) -> list:
        try:
            r = self.s.get(
                f"{TEMPMAIL_API}/mails",
                params={"email": self.receiver},
                timeout=10,
            )
            r.raise_for_status()
            d = r.json()
            return d.get("mail_list", []) if d.get("result") else []
        except Exception as e:
            log.warning(f"tempmail 列表失败: {e}")
            return []

    def get_detail(self, mail_id) -> dict:
        r = self.s.get(
            f"{TEMPMAIL_API}/mails/{mail_id}",
            params={"email": self.receiver},
            timeout=10,
        )
        r.raise_for_status()
        return r.json()

    def wait_verification_code(self, target_email: str, known_mail_ids: set) -> str:
        """轮询直到收到 Clerk 的验证码邮件（主题 "XXXXXX is your verification code"）。"""
        deadline = time.time() + MAIL_TIMEOUT
        target_low = target_email.lower()

        while time.time() < deadline:
            try:
                mails = self.get_mails()
                for m in mails:
                    mid = m.get("mail_id")
                    if mid in known_mail_ids:
                        continue
                    subject = (m.get("subject") or "").strip()

                    # 先用主题做低成本匹配（Clerk 总在主题带验证码）
                    m_sub = CODE_SUBJECT_RE.match(subject)
                    if not m_sub:
                        continue

                    # 获取详情确认发给我们这个子地址
                    detail = self.get_detail(mid)
                    to_val = (detail.get("to") or "").lower()
                    if target_low not in to_val:
                        continue

                    code = m_sub.group(1)
                    known_mail_ids.add(mid)
                    return code
            except Exception as e:
                log.warning(f"轮询异常: {e}")
            time.sleep(MAIL_INTERVAL)

        raise TimeoutError(f"等待验证码超时（{MAIL_TIMEOUT}s）")


# ============================================================
# 浏览器指纹 —— 固定 Chrome 147（mac 当前实际版本），避免 UA / 真实版本
# 不匹配被 Cloudflare / Clerk 识别为 bot
# ============================================================
def random_fingerprint() -> dict:
    # Chrome major 版本必须和实机真实 Chrome 一致
    chrome_ver = f"147.0.{random.randint(7700, 7800)}.{random.randint(100, 200)}"
    mac_ver = random.choice([
        "10_15_7", "14_5_0", "15_0_0", "15_1_0", "15_3_0",
    ])
    w, h = random.choice([(1280, 800), (1366, 768), (1440, 900), (1536, 864)])
    ua = (
        f"Mozilla/5.0 (Macintosh; Intel Mac OS X {mac_ver}) "
        f"AppleWebKit/537.36 (KHTML, like Gecko) "
        f"Chrome/{chrome_ver} Safari/537.36"
    )
    return {"ua": ua, "window": f"{w},{h}", "chrome_ver": chrome_ver}


def random_password() -> str:
    """生成符合常规复杂度的密码（大小写+数字+符号，≥14位）"""
    alphabet = string.ascii_letters + string.digits
    core = "".join(random.choices(alphabet, k=12))
    return f"CtoProxy!{core}"


# ============================================================
# 浏览器管理
# ============================================================
class CtoBrowser:
    def __init__(self, headless: bool = True):
        self.headless = headless
        self.driver = None
        self._tmp_profile = None

    def start(self):
        fp = random_fingerprint()
        self._tmp_profile = tempfile.mkdtemp(prefix="cto_chrome_")
        opts = Options()

        # eager: Selenium 在 DOMContentLoaded 时就认为页面 ready，
        # 不等 Turnstile / 广告等后台资源加载完（避免 find_element 被无限阻塞）
        opts.page_load_strategy = "eager"

        # 精简的 Chrome flags —— 更多 flags 会被 Clerk / Cloudflare 识别成 bot
        if self.headless:
            opts.add_argument("--headless=new")
        opts.add_argument("--no-sandbox")
        opts.add_argument("--disable-dev-shm-usage")
        opts.add_argument(f"--user-data-dir={self._tmp_profile}")
        opts.add_argument(f"--window-size={fp['window']}")
        opts.add_argument(f"--user-agent={fp['ua']}")
        opts.add_argument("--no-first-run")
        opts.add_argument("--no-default-browser-check")
        opts.add_argument("--log-level=3")

        # 反检测（关键：去掉 automation switches + 覆盖 webdriver）
        opts.add_experimental_option(
            "excludeSwitches", ["enable-automation", "enable-logging"]
        )
        opts.add_experimental_option("useAutomationExtension", False)
        opts.add_argument("--disable-blink-features=AutomationControlled")

        # 只关掉通知，不动其它内容
        opts.add_experimental_option("prefs", {
            "profile.default_content_setting_values.notifications": 2,
            "credentials_enable_service": False,
            "profile.password_manager_enabled": False,
        })

        self.driver = webdriver.Chrome(
            service=ChromeService(log_output=os.devnull), options=opts
        )
        self.driver.set_page_load_timeout(PAGE_LOAD_TIMEOUT)
        self.driver.set_script_timeout(30)

        # 反检测脚本 —— 只覆盖 webdriver 字段，其它属性伪造反而容易被 Clerk 的
        # 指纹检测识别为 "非一致环境" 而屏蔽表单
        self.driver.execute_cdp_cmd(
            "Page.addScriptToEvaluateOnNewDocument",
            {"source": "Object.defineProperty(navigator,'webdriver',{get:()=>undefined});"},
        )

        log.info(f"  指纹: Chrome/{fp['chrome_ver'].split('.')[0]} 窗口={fp['window']}")

    def quit(self):
        if self.driver:
            try:
                self.driver.quit()
            except Exception:
                pass
            self.driver = None
        if self._tmp_profile and os.path.exists(self._tmp_profile):
            try:
                shutil.rmtree(self._tmp_profile, ignore_errors=True)
            except Exception:
                pass
            self._tmp_profile = None
        gc.collect()

    # --------------------------------------------------------
    # 1) 填第一屏表单（邮箱 + 密码 + TOS + Continue）
    # --------------------------------------------------------
    def submit_signup_form(self, email: str, password: str) -> bool:
        log.info(f"打开注册页: {SIGNUP_URL}")
        self.driver.get(SIGNUP_URL)

        # Clerk 是 React SPA + Cloudflare JS 挑战，前 15-30 秒可能是空 body。
        # 用 JS 轮询等 input 真正挂载（比 Selenium find_element 可靠）。
        log.info("  等待 Clerk 表单渲染（JS 轮询，最长 60s）...")
        email_input_ready = False
        for sec in range(WAIT_TIMEOUT):
            try:
                n = self.driver.execute_script(
                    "return document.getElementById('emailAddress-field') ? 1 : 0"
                )
                if n == 1:
                    log.info(f"  ✅ 表单在 {sec}s 时出现")
                    email_input_ready = True
                    break
            except Exception:
                pass
            time.sleep(1)
        if not email_input_ready:
            log.error(f"  ❌ Clerk 表单 {WAIT_TIMEOUT}s 内未渲染（可能 Cloudflare 拦截）")
            return False

        try:
            # 用 JS 填充 React controlled input（必须触发 native setter + input 事件）
            fill_result = self.driver.execute_script(
                """
                function setNativeValue(el, value) {
                  const proto = Object.getPrototypeOf(el);
                  const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
                  setter.call(el, value);
                  el.dispatchEvent(new Event('input', { bubbles: true }));
                  el.dispatchEvent(new Event('change', { bubbles: true }));
                }
                const email = document.getElementById('emailAddress-field');
                const pwd   = document.getElementById('password-field');
                const tos   = document.getElementById('legalAccepted-field');
                if (!email || !pwd) return 'missing';
                email.focus(); setNativeValue(email, arguments[0]);
                pwd.focus();   setNativeValue(pwd, arguments[1]);
                if (tos && !tos.checked) tos.click();
                return 'ok';
                """,
                email, password,
            )
            if fill_result != "ok":
                log.error(f"  ❌ JS 填表失败: {fill_result}")
                return False
            log.info("  表单已填写（JS setter + input event）")
            time.sleep(2)

            # 等 invisible Turnstile 跑完（后台 3-5 秒）
            time.sleep(3)

            # 用 JS 直接点击 submit 按钮（Clerk 的 Continue）
            click_result = self.driver.execute_script(
                """
                const btn = document.querySelector('button[type="submit"]');
                if (!btn) return 'no-btn';
                if (btn.disabled) return 'disabled';
                btn.click();
                return 'clicked: ' + btn.textContent.trim().slice(0, 30);
                """
            )
            log.info(f"  提交按钮: {click_result}")
            if click_result.startswith(("no-btn", "disabled")):
                return False

            # 等跳转到 verify-email-address 或表单切换成验证码输入
            # 同时实时打印状态，便于调试
            deadline = time.time() + 45  # 捕获 captcha 挑战 + 后端处理 + 跳转
            last_status = ""
            while time.time() < deadline:
                time.sleep(2)
                try:
                    url = self.driver.current_url
                except WebDriverException:
                    continue

                # 命中条件：URL 跳转到 verify-email
                if "verify-email" in url:
                    log.info(f"  ✅ 进入验证码页: {url}")
                    return True

                # 命中条件：页面上出现了 6 位验证码输入框
                inputs = self.driver.find_elements(
                    By.CSS_SELECTOR,
                    'input[autocomplete="one-time-code"], input[inputmode="numeric"]',
                )
                if inputs:
                    log.info(f"  ✅ 找到验证码输入框 (URL={url})")
                    return True

                # 检查是否有错误提示
                try:
                    err_el = self.driver.find_elements(
                        By.CSS_SELECTOR,
                        '[data-localization-key*="error" i], '
                        '[role="alert"], '
                        '.cl-formFieldErrorText',
                    )
                    if err_el:
                        msg = err_el[0].text.strip()
                        if msg and msg != last_status:
                            last_status = msg
                            log.warning(f"  Clerk 提示: {msg}")
                except Exception:
                    pass

                elapsed = int(deadline - time.time())
                log.info(f"  [等待... remain={elapsed}s] URL={url[:80]}")

            # 超时：dump 页面信息便于排查
            try:
                page_text = self.driver.execute_script(
                    "return document.body.innerText.slice(0, 1000)"
                )
                log.error(f"  超时！页面片段:\n{page_text}")
            except Exception:
                pass
            return False

        except TimeoutException as e:
            log.error(f"  表单元素等待超时: {type(e).__name__}")
            try:
                log.error(f"  当前 URL: {self.driver.current_url}")
                log.error(f"  Page title: {self.driver.title}")
            except Exception:
                pass
            return False
        except Exception as e:
            log.error(f"  注册表单提交失败: {e}")
            return False

    # --------------------------------------------------------
    # 2) 填验证码 + 等跳转
    # --------------------------------------------------------
    def submit_verification_code(self, code: str) -> bool:
        log.info(f"填入验证码: {code}")
        try:
            # Clerk 可能渲染 6 个单独的 input 或一个 6 位 input
            single = self.driver.find_elements(
                By.CSS_SELECTOR,
                'input[inputmode="numeric"], input[autocomplete="one-time-code"]',
            )
            if len(single) == 1:
                single[0].clear()
                single[0].send_keys(code)
            else:
                # 逐位
                inputs = self.driver.find_elements(
                    By.CSS_SELECTOR, 'input[maxlength="1"]'
                )
                if len(inputs) >= 6:
                    for i, ch in enumerate(code):
                        inputs[i].send_keys(ch)
                        time.sleep(0.05)
                elif single:
                    single[0].clear()
                    single[0].send_keys(code)
                else:
                    log.error("  找不到验证码输入框")
                    return False
            time.sleep(1.0)

            # Clerk 一般是自动提交（填满就提交）；再找一下 Continue 兜底
            try:
                btn = self.driver.find_element(
                    By.XPATH, '//button[@type="submit"]'
                )
                if btn.is_enabled():
                    self.driver.execute_script("arguments[0].click();", btn)
            except Exception:
                pass

            # 等跳转回 cto.new 首页
            deadline = time.time() + VERIFY_TIMEOUT
            while time.time() < deadline:
                url = self.driver.current_url
                if url.startswith("https://cto.new/") and "/sign-up" not in url:
                    log.info(f"  注册完成，跳转: {url}")
                    return True
                time.sleep(1.5)
            log.error(f"  验证后跳转超时，最终 URL: {self.driver.current_url}")
            return False
        except Exception as e:
            log.error(f"  验证码提交失败: {e}")
            return False

    # --------------------------------------------------------
    # 3) 从浏览器提取 Clerk session + engine 账号信息
    # --------------------------------------------------------
    def extract_credentials(self) -> dict:
        """从浏览器提取完整凭据：
           - JS：拿 Clerk session/user、engine team（用 JWT 调 /current-user）
           - CDP：Network.getAllCookies 直接拿所有域的 cookie（含 HTTP-only 的 __client）
           - 自检：用拿到的 __client cookie 实际调一次刷新，确保反代能用
        """
        # 先确保留在 cto.new 主页（Clerk 需要加载）
        cur = ""
        try:
            cur = self.driver.current_url or ""
        except Exception:
            pass
        if not cur.startswith("https://cto.new/"):
            self.driver.get(AFTER_SIGNUP_URL)
            time.sleep(3)

        info = self.driver.execute_async_script("""
            const done = arguments[arguments.length - 1];
            (async () => {
              try {
                for (let i = 0; i < 40; i++) {
                  if (window.Clerk && window.Clerk.session && window.Clerk.user) break;
                  await new Promise(r => setTimeout(r, 500));
                }
                if (!window.Clerk || !window.Clerk.session) {
                  done({ error: 'clerk not ready' });
                  return;
                }
                const session = window.Clerk.session;
                const user = window.Clerk.user;
                const token = await session.getToken();
                let cu = {};
                try {
                  const r = await fetch('https://api.enginelabs.ai/current-user', {
                    headers: { Authorization: 'Bearer ' + token }
                  });
                  cu = await r.json();
                } catch(e) { cu = { _err: String(e) }; }
                let ws = [];
                try {
                  const r = await fetch('https://api.enginelabs.ai/workspaces?pageSize=100', {
                    headers: { Authorization: 'Bearer ' + token }
                  });
                  const data = await r.json();
                  ws = data.workspaces || [];
                } catch(e) {}
                done({
                  email: (user.emailAddresses || [])[0]?.emailAddress || '',
                  clerk_user_id: user.id,
                  clerk_session_id: session.id,
                  clerk_org_id: session.lastActiveOrganizationId || '',
                  clerk_jwt: token,
                  current_user: cu,
                  workspaces: ws,
                });
              } catch(e) {
                done({ error: String(e) });
              }
            })();
        """)
        if not info:
            return {"error": "no info returned"}
        if info.get("error"):
            return info

        # 用 CDP 取所有域的 cookie（跨域 + HTTP-only）
        cookie_map = {}
        try:
            result = self.driver.execute_cdp_cmd("Network.getAllCookies", {})
            for c in result.get("cookies", []):
                name = c.get("name", "")
                val = c.get("value", "")
                dom = c.get("domain", "")
                if not name or not val:
                    continue
                # 同名 cookie 可能在多个域存在，优先取 cto.new / clerk.cto.new 域的
                if name not in cookie_map or "cto.new" in dom or "clerk" in dom:
                    cookie_map[name] = val
        except Exception as e:
            log.warning(f"  CDP getAllCookies 失败: {e}")
            return {"error": f"cdp failed: {e}"}

        info["cookies"] = cookie_map
        return info

    # --------------------------------------------------------
    # 4) 自检：用拿到的 __client cookie 调一次 Clerk refresh 验证能用
    # --------------------------------------------------------
    @staticmethod
    def verify_refresh(client_cookie: str, session_id: str) -> bool:
        if not client_cookie or not session_id:
            return False
        try:
            r = requests.post(
                f"https://clerk.cto.new/v1/client/sessions/{session_id}/tokens"
                f"?__clerk_api_version=2025-11-10&_clerk_js_version=6.7.3",
                headers={
                    "Origin": "https://cto.new",
                    "Referer": "https://cto.new/",
                    "User-Agent": (
                        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
                        "AppleWebKit/537.36 (KHTML, like Gecko) "
                        "Chrome/147.0.0.0 Safari/537.36"
                    ),
                    "Cookie": f"__client={client_cookie}",
                },
                timeout=10,
            )
            if r.status_code != 200:
                return False
            jwt = r.json().get("jwt", "")
            return len(jwt) > 100
        except Exception:
            return False


# ============================================================
# 单个账号注册
# ============================================================
def register_one(idx: int, mail: TempMailPlus,
                 headless: bool = True,
                 known_mail_ids: set = None,
                 max_retry: int = 2) -> dict:
    email = f"{idx}@{EMAIL_DOMAIN}"
    password = random_password()
    log.info("=" * 60)
    log.info(f"注册 #{idx}: {email}")
    log.info("=" * 60)

    for attempt in range(1, max_retry + 1):
        if attempt > 1:
            log.warning(f"  第 {attempt} 次重试...")
            time.sleep(5)

        browser = CtoBrowser(headless=headless)
        try:
            browser.start()

            # Step 1: 提交邮箱+密码
            if not browser.submit_signup_form(email, password):
                continue

            # Step 2: 从邮箱取验证码
            try:
                code = mail.wait_verification_code(
                    email, known_mail_ids or set()
                )
                log.info(f"  收到验证码: {code}")
            except TimeoutError as e:
                log.error(f"  {e}")
                continue

            # Step 3: 提交验证码
            if not browser.submit_verification_code(code):
                continue

            # Step 4: 提取凭据
            creds = browser.extract_credentials()
            if creds.get("error"):
                log.error(f"  凭据提取失败: {creds['error']}")
                continue

            # 从 current_user 提取 engine 信息
            cu = (creds.get("current_user") or {}).get("currentUser", {}) or {}
            team = cu.get("team", {}) or {}
            workspaces = creds.get("workspaces") or []
            default_ws = workspaces[0] if workspaces else {}

            # 从完整 cookie 集合里取 Clerk 关键 cookie
            cookies = creds.get("cookies") or {}
            ck_client = cookies.get("__client", "")           # HTTP-only，刷新 JWT 的核心
            ck_session = cookies.get("__session", creds.get("clerk_jwt", ""))
            ck_client_uat = cookies.get("__client_uat", "")
            session_id = creds.get("clerk_session_id", "")

            if not ck_client:
                log.error("  ❌ 没拿到 __client cookie（反代将无法刷新 JWT），重试...")
                continue

            # 必须字段校验：team_id / engine_user_id 缺一不可
            if not cu.get("id") or not team.get("id"):
                log.error(f"  ❌ engine 用户信息不全，cu={cu}")
                continue

            # 自检：用 __client cookie 实际刷一次 JWT，确认反代能用
            if not CtoBrowser.verify_refresh(ck_client, session_id):
                log.error("  ❌ __client cookie 刷新自检失败（拿到的 cookie 不能用），重试...")
                continue

            acc = {
                "email": email,
                "password": password,
                "clerk_user_id": creds.get("clerk_user_id", ""),
                "clerk_session_id": session_id,
                "clerk_org_id": creds.get("clerk_org_id", ""),
                "clerk_client_cookie": ck_client,
                "clerk_session_cookie": ck_session,
                "clerk_client_uat": ck_client_uat,
                "engine_user_id": cu.get("id", ""),
                "engine_team_id": team.get("id", ""),
                "engine_api_key": team.get("apiKey", ""),
                "default_workspace_id": default_ws.get("id", ""),
                "registered_at": datetime.utcnow().isoformat(timespec="seconds"),
                "ws_token_suffix": (
                    f"{creds.get('clerk_user_id', '')}:{creds.get('clerk_org_id', '')}"
                ),
            }
            log.info(
                f"  ✅ 注册+自检成功! team_id={acc['engine_team_id'][:8]}... "
                f"api_key={acc['engine_api_key'][:8]}... "
                f"client={ck_client[:20]}..."
            )
            return acc
        except Exception as e:
            log.error(f"  异常: {e}")
            continue
        finally:
            browser.quit()
            gc.collect()

    return {}


def _cookie_get(cookie_str: str, name: str) -> str:
    for part in (cookie_str or "").split(";"):
        part = part.strip()
        if part.startswith(name + "="):
            return part[len(name) + 1:]
    return ""


# ============================================================
# 持久化
# ============================================================
def save_account(acc: dict, path: Path):
    with open(path, "a", encoding="utf-8") as f:
        f.write(json.dumps(acc, ensure_ascii=False) + "\n")
    log.info(f"已写入 {path.name}: {acc['email']}")


# ============================================================
# 主入口
# ============================================================
def main():
    ap = argparse.ArgumentParser(description="cto.new 批量注册机")
    ap.add_argument("--start", type=int, default=1, help="起始序号")
    ap.add_argument("--count", type=int, default=1, help="注册数量")
    # Clerk invisible Turnstile 在真实浏览器下最稳，默认非 headless
    ap.add_argument("--headless", action="store_true",
                    help="启用无头模式（Turnstile 可能失败，生产环境不推荐）")
    ap.add_argument("--output", type=str, default=str(OUTPUT_FILE),
                    help="输出 jsonl 路径")
    ap.add_argument("--gap-min", type=float, default=4.0,
                    help="两次注册最小间隔秒")
    ap.add_argument("--gap-max", type=float, default=9.0,
                    help="两次注册最大间隔秒")
    args = ap.parse_args()

    out = Path(args.output)
    hl = args.headless

    log.info("=" * 60)
    log.info("cto.new 批量注册机")
    log.info(f"  目标序号: {args.start} ~ {args.start + args.count - 1}")
    log.info(f"  headless: {hl}  输出: {out}")
    log.info("=" * 60)

    mail = TempMailPlus(TEMPMAIL_RECEIVER)

    try:
        known = {m.get("mail_id") for m in mail.get_mails()}
        log.info(f"  收件箱已有 {len(known)} 封邮件（注册期间忽略）")
    except Exception:
        known = set()

    ok = fail = 0
    for i in range(args.start, args.start + args.count):
        acc = register_one(i, mail, hl, known)
        if acc and acc.get("engine_team_id"):
            save_account(acc, out)
            ok += 1
        else:
            fail += 1
            log.warning(f"  #{i} 注册失败")

        if i < args.start + args.count - 1:
            delay = random.uniform(args.gap_min, args.gap_max)
            log.info(f"  休息 {delay:.1f}s 再继续...")
            time.sleep(delay)

        if i % 5 == 0:
            gc.collect()

    log.info("=" * 60)
    log.info(f"完成! 成功 {ok} / 失败 {fail}")
    log.info("=" * 60)


if __name__ == "__main__":
    main()
