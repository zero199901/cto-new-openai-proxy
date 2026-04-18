#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
cto.new 批量注册机 —— Playwright 版（备选）
===========================================
首选请用 cto_register_sb.py（SeleniumBase UC Mode，对 Cloudflare 严格模式
绕过率最高）。本文件保留作为环境缺 UC Mode 时的备选。

与 cto_register_sb.py 共享 mail_client.MailClient 作为邮箱层，默认使用
GPTMail（https://mail.chatgpt.org.uk）。

用法：
  python3 cto_register_pw.py --count 5
  python3 cto_register_pw.py --count 1 --show               # 显示浏览器
  python3 cto_register_pw.py --count 1 --provider tempmail  # 兜底 tempmail.plus
"""
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

from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

from mail_client import MailClient

BASE_DIR = Path(__file__).resolve().parent

logging.basicConfig(
    level=logging.INFO,
    format="[%(asctime)s] %(levelname)s - %(message)s",
    datefmt="%H:%M:%S",
    handlers=[
        logging.StreamHandler(sys.stdout),
        logging.FileHandler(BASE_DIR / "cto_register_pw.log", encoding="utf-8"),
    ],
)
log = logging.getLogger(__name__)

SIGNUP_URL = "https://accounts.cto.new/sign-up"
AFTER_SIGNUP_URL = "https://cto.new/"
OUTPUT_FILE = BASE_DIR / "accounts.jsonl"

FORM_WAIT_SEC = 60       # Clerk 表单渲染最长等待
CAPTCHA_WAIT_SEC = 15    # Turnstile invisible 后台执行等待
MAIL_TIMEOUT = 120
VERIFY_TIMEOUT = 60


def random_password() -> str:
    core = "".join(random.choices(string.ascii_letters + string.digits, k=12))
    return f"CtoProxy!{core}"


def verify_refresh(client_cookie: str, session_id: str) -> bool:
    """用 __client cookie 实际调一次 Clerk refresh，验证反代可用"""
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
        return r.status_code == 200 and len(r.json().get("jwt", "")) > 100
    except Exception:
        return False


# ============================================================
# 单个账号注册
# ============================================================
def register_one(pw, context, idx: int, mail: MailClient) -> dict:
    # -------- 分配新邮箱 --------
    email = mail.generate()
    password = random_password()
    log.info("=" * 60)
    log.info(f"注册 #{idx}: {email}")
    log.info("=" * 60)
    known_mail_ids = mail.list_ids(email)
    log.info(f"  收件箱已有 {len(known_mail_ids)} 封邮件（忽略）")

    page = context.new_page()
    try:
        log.info(f"  打开 {SIGNUP_URL}")
        page.goto(SIGNUP_URL, wait_until="domcontentloaded", timeout=60_000)

        # 等表单渲染（Cloudflare JS 挑战需要时间）
        log.info(f"  等 Clerk 表单渲染（最长 {FORM_WAIT_SEC}s）...")
        try:
            page.wait_for_selector("input#emailAddress-field",
                                   state="visible", timeout=FORM_WAIT_SEC * 1000)
        except PWTimeout:
            log.error("  ❌ 表单渲染超时")
            return {}

        # 等 Turnstile widget 加载完毕（Clerk 调用 window.turnstile.render 后才显示按钮 enabled）
        log.info("  等 Turnstile widget 就绪...")
        for _ in range(20):
            ready = page.evaluate("""() => {
                // Clerk 把 Turnstile 挂在 form 里，widget 渲染后 window.turnstile 存在
                const has_api = typeof window.turnstile !== 'undefined';
                // Turnstile iframe 会出现在页面上
                const iframe = document.querySelector('iframe[src*="challenges.cloudflare.com"]');
                return has_api || !!iframe;
            }""")
            if ready:
                break
            time.sleep(0.5)
        log.info("  Turnstile 已挂载")

        # 填表
        page.fill("input#emailAddress-field", email)
        page.fill("input#password-field", password)
        try:
            box = page.locator("input#legalAccepted-field")
            if not box.is_checked():
                box.check()
        except Exception:
            pass
        time.sleep(1)

        # 关键：让 Turnstile 有时间执行 invisible 挑战
        # 真人填表也需要 5-10 秒，Turnstile 会在这期间后台跑完
        log.info("  给 Turnstile 12 秒执行时间...")
        time.sleep(12)

        # 提交表单：优先点可见的 Continue 按钮，退回按 Enter
        # 注：Clerk 表单里有隐藏的 type=submit，不能用 .first
        log.info("  提交表单")
        clicked = False
        for sel in [
            'button[type="submit"]:visible:has-text("Continue")',
            'button:visible:has-text("Continue")',
            '.cl-formButtonPrimary',
        ]:
            try:
                loc = page.locator(sel)
                if loc.count() > 0:
                    loc.first.click(timeout=5_000)
                    clicked = True
                    log.info(f"  点击按钮成功: selector={sel}")
                    break
            except Exception as e:
                log.debug(f"  selector {sel} 失败: {e}")
        if not clicked:
            log.info("  所有按钮 selector 失败，退回 Enter 键提交")
            page.locator("input#password-field").press("Enter")

        # 等跳转到 verify-email-address 或 URL 包含 /verify
        log.info("  等待跳转到验证码页...")
        try:
            page.wait_for_url(re.compile(r"verify"), timeout=45_000)
        except PWTimeout:
            log.error(f"  ❌ 跳转超时，当前 URL: {page.url}")
            # dump 错误提示
            try:
                err = page.locator("[role=alert], .cl-formFieldErrorText").first.text_content(timeout=1000)
                log.error(f"  Clerk 提示: {err}")
            except Exception:
                pass
            return {}
        log.info(f"  ✅ 进入验证码页: {page.url}")

        # 取验证码
        code = mail.wait_code(email, known_mail_ids, timeout=MAIL_TIMEOUT)
        log.info(f"  收到验证码: {code}")

        # 填验证码（单输入框或分格，两种都尝试）
        time.sleep(1)
        try:
            otp = page.locator('input[autocomplete="one-time-code"]').first
            otp.fill(code)
        except Exception:
            boxes = page.locator('input[maxlength="1"]')
            cnt = boxes.count()
            if cnt >= len(code):
                for i, ch in enumerate(code):
                    boxes.nth(i).fill(ch)
                    time.sleep(0.03)

        # Clerk 自动提交（填满自动 submit）；兜底：若按钮仍可点则再点一次
        try:
            page.locator('button[type="submit"]').click(timeout=3000)
        except Exception:
            pass

        # 等跳转回 cto.new/
        log.info("  等待跳转回 cto.new/...")
        try:
            page.wait_for_url(re.compile(r"^https://cto\.new/"),
                              timeout=VERIFY_TIMEOUT * 1000)
        except PWTimeout:
            log.error(f"  ❌ 验证后跳转超时，URL: {page.url}")
            return {}

        # 等 Clerk session ready
        log.info("  等 Clerk session 注入...")
        for _ in range(30):
            ready = page.evaluate(
                "() => !!(window.Clerk && window.Clerk.session && window.Clerk.user)"
            )
            if ready:
                break
            time.sleep(0.5)

        # 拿 Clerk 信息 + engine current-user + workspaces
        info = page.evaluate("""
            async () => {
              const sess = window.Clerk.session;
              const user = window.Clerk.user;
              const tok = await sess.getToken();
              const H = { Authorization: 'Bearer ' + tok };
              let cu = {}, ws = [];
              try { cu = await (await fetch('https://api.enginelabs.ai/current-user', {headers: H})).json(); } catch(e){}
              try {
                const d = await (await fetch('https://api.enginelabs.ai/workspaces?pageSize=100', {headers: H})).json();
                ws = d.workspaces || [];
              } catch(e){}
              return {
                email: user.emailAddresses?.[0]?.emailAddress || '',
                clerk_user_id: user.id,
                clerk_session_id: sess.id,
                clerk_org_id: sess.lastActiveOrganizationId || '',
                clerk_jwt: tok,
                current_user: cu,
                workspaces: ws,
              };
            }
        """)

        # 取跨域 cookie（含 HTTP-only __client）
        all_cookies = context.cookies()
        cookie_map = {}
        for c in all_cookies:
            dom = c.get("domain", "")
            name = c["name"]
            if "cto.new" in dom or "clerk" in dom:
                if name not in cookie_map:
                    cookie_map[name] = c["value"]
        ck_client = cookie_map.get("__client", "")
        ck_session = cookie_map.get("__session", info.get("clerk_jwt", ""))
        ck_client_uat = cookie_map.get("__client_uat", "")

        if not ck_client:
            log.error("  ❌ 没拿到 __client cookie")
            return {}

        session_id = info.get("clerk_session_id", "")
        if not verify_refresh(ck_client, session_id):
            log.error("  ❌ __client cookie 自检失败")
            return {}

        cu = (info.get("current_user") or {}).get("currentUser", {}) or {}
        team = cu.get("team", {}) or {}
        workspaces = info.get("workspaces") or []
        default_ws = workspaces[0] if workspaces else {}

        acc = {
            "email": email,
            "password": password,
            "clerk_user_id": info.get("clerk_user_id", ""),
            "clerk_session_id": session_id,
            "clerk_org_id": info.get("clerk_org_id", ""),
            "clerk_client_cookie": ck_client,
            "clerk_session_cookie": ck_session,
            "clerk_client_uat": ck_client_uat,
            "engine_user_id": cu.get("id", ""),
            "engine_team_id": team.get("id", ""),
            "engine_api_key": team.get("apiKey", ""),
            "default_workspace_id": default_ws.get("id", ""),
            "registered_at": datetime.utcnow().isoformat(timespec="seconds"),
            "ws_token_suffix": f"{info.get('clerk_user_id','')}:{info.get('clerk_org_id','')}",
        }
        log.info(
            f"  ✅ 注册+自检通过 team_id={acc['engine_team_id'][:8]}... "
            f"client={ck_client[:18]}..."
        )
        return acc

    except Exception as e:
        log.error(f"  注册异常: {e}")
        return {}
    finally:
        try:
            page.close()
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
    ap = argparse.ArgumentParser(description="cto.new 批量注册（Playwright 版）")
    ap.add_argument("--count", type=int, default=1, help="要注册的账号数量")
    ap.add_argument("--show", action="store_true", help="显示浏览器（默认 headless）")
    ap.add_argument("--output", type=str, default=str(OUTPUT_FILE))
    ap.add_argument("--provider", type=str, default="gptmail",
                    choices=["gptmail", "tempmail"],
                    help="邮箱服务商（默认 gptmail）")
    ap.add_argument("--gap-min", type=float, default=4.0)
    ap.add_argument("--gap-max", type=float, default=9.0)
    args = ap.parse_args()

    out = Path(args.output)
    log.info("=" * 60)
    log.info(f"cto.new 批量注册机 (Playwright 备选版)")
    log.info(f"  数量: {args.count}  show: {args.show}  邮箱商: {args.provider}")
    log.info(f"  输出: {out}")
    log.info("=" * 60)

    mail = MailClient(provider=args.provider)

    ok = fail = 0
    with sync_playwright() as pw:
        # Playwright 自带的 chromium 做了更完善的反检测（比系统 Chrome 更能过 Turnstile）
        browser = pw.chromium.launch(
            headless=not args.show,
            args=[
                "--disable-blink-features=AutomationControlled",
                "--no-first-run",
                "--no-default-browser-check",
            ],
        )
        for i in range(args.count):
            # 每个账号用新 context（新 session，互不干扰）
            context = browser.new_context(
                user_agent=(
                    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
                    "AppleWebKit/537.36 (KHTML, like Gecko) "
                    "Chrome/147.0.7727.101 Safari/537.36"
                ),
                viewport={"width": 1366, "height": 900},
                locale="en-US",
            )
            # 反检测：隐藏 navigator.webdriver
            context.add_init_script(
                "Object.defineProperty(navigator,'webdriver',{get:()=>undefined});"
            )
            try:
                acc = register_one(pw, context, i + 1, mail)
                if acc and acc.get("engine_team_id"):
                    save_account(acc, out)
                    ok += 1
                else:
                    fail += 1
                    log.warning(f"  #{i + 1} 失败")
            finally:
                context.close()

            if i < args.count - 1:
                delay = random.uniform(args.gap_min, args.gap_max)
                log.info(f"  休息 {delay:.1f}s ...")
                time.sleep(delay)

        browser.close()

    log.info("=" * 60)
    log.info(f"完成! 成功 {ok} / 失败 {fail}")
    log.info("=" * 60)


if __name__ == "__main__":
    main()
