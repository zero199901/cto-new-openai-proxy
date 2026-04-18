#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
cto.new 登录刷新器 —— Playwright 版
====================================
当 accounts.jsonl 里某个账号的 __client cookie 失效（反代 401/登录过久）时，
用已存的密码重新登录一次，抓新的 __client cookie 写回 accounts.jsonl。

注册脚本受 Cloudflare Turnstile 风控影响严重，但**登录**的风控明显更松。
此脚本当"cookie 断奶"时的快速恢复工具非常好用，远比重新注册稳定。

用法：
  python3 cto_login_refresh.py              # 刷新所有账号的 cookie
  python3 cto_login_refresh.py --email 1@your-domain.email  # 只刷一个
  python3 cto_login_refresh.py --show       # 带界面调试
"""
import os
import sys
import re
import json
import time
import logging
import argparse
import requests
from pathlib import Path

from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

BASE_DIR = Path(__file__).resolve().parent
ACCOUNTS_FILE = BASE_DIR / "accounts.jsonl"

logging.basicConfig(
    level=logging.INFO,
    format="[%(asctime)s] %(levelname)s - %(message)s",
    datefmt="%H:%M:%S",
)
log = logging.getLogger(__name__)

SIGNIN_URL = "https://accounts.cto.new/sign-in"
AFTER_SIGNIN_URL = "https://cto.new/"

UA = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.7727.101 Safari/537.36"
)


def verify_refresh(client_cookie: str, session_id: str) -> bool:
    """直接调 Clerk 的 refresh 端点检查 __client 是否能换 JWT"""
    if not client_cookie or not session_id:
        return False
    try:
        r = requests.post(
            f"https://clerk.cto.new/v1/client/sessions/{session_id}/tokens"
            f"?__clerk_api_version=2025-11-10&_clerk_js_version=6.7.3",
            headers={
                "Origin": "https://cto.new",
                "Referer": "https://cto.new/",
                "User-Agent": UA,
                "Cookie": f"__client={client_cookie}",
            },
            timeout=10,
        )
        return r.status_code == 200 and len(r.json().get("jwt", "")) > 100
    except Exception:
        return False


def login_one(context, email: str, password: str) -> dict:
    """在 context 里登录一次，返回新的 cookie / 会话信息"""
    page = context.new_page()
    try:
        log.info(f"  访问 {SIGNIN_URL}")
        page.goto(SIGNIN_URL, wait_until="domcontentloaded", timeout=60_000)

        page.wait_for_selector("input#identifier-field", state="visible", timeout=60_000)
        log.info(f"  填入 {email}")
        page.fill("input#identifier-field", email)
        page.fill("input#password-field", password)
        time.sleep(1)

        log.info("  提交")
        clicked = False
        for sel in [
            'button:visible:has-text("Continue")',
            'button[type="submit"]:visible',
            ".cl-formButtonPrimary",
        ]:
            try:
                loc = page.locator(sel)
                if loc.count() > 0:
                    loc.first.click(timeout=5_000)
                    clicked = True
                    break
            except Exception:
                pass
        if not clicked:
            page.locator("input#password-field").press("Enter")

        # 等跳回 cto.new/
        page.wait_for_url(re.compile(r"^https://cto\.new/"), timeout=45_000)
        log.info(f"  ✅ 登录成功: {page.url}")

        # 等 Clerk 注入
        for _ in range(30):
            ready = page.evaluate(
                "() => !!(window.Clerk && window.Clerk.session && window.Clerk.user)"
            )
            if ready:
                break
            time.sleep(0.5)

        info = page.evaluate("""
            async () => {
              const sess = window.Clerk.session;
              const user = window.Clerk.user;
              const tok = await sess.getToken();
              return {
                email: user.emailAddresses?.[0]?.emailAddress || '',
                clerk_user_id: user.id,
                clerk_session_id: sess.id,
                clerk_org_id: sess.lastActiveOrganizationId || '',
                clerk_jwt: tok,
              };
            }
        """)

        # CDP 取 cookie（含 __client HTTP-only）
        cdp = context.new_cdp_session(page)
        all_cookies = cdp.send("Network.getAllCookies")
        ck_client = ""
        ck_session = ""
        ck_client_uat = ""
        for c in all_cookies.get("cookies", []):
            name = c.get("name", "")
            dom = c.get("domain", "")
            val = c.get("value", "")
            if name == "__client" and "clerk" in dom and not ck_client:
                ck_client = val
            elif name == "__session" and dom == "cto.new" and not ck_session:
                ck_session = val
            elif name == "__client_uat" and not ck_client_uat and not name.endswith(("_LUZhIdZf", "_4rA5aJ5P")):
                ck_client_uat = val

        info["clerk_client_cookie"] = ck_client
        info["clerk_session_cookie"] = ck_session or info.get("clerk_jwt", "")
        info["clerk_client_uat"] = ck_client_uat
        return info

    except PWTimeout as e:
        log.error(f"  ❌ 登录超时: {e}")
        return {}
    except Exception as e:
        log.error(f"  ❌ 登录异常: {e}")
        return {}
    finally:
        try:
            page.close()
        except Exception:
            pass


def refresh_account(pw, acc: dict, show: bool) -> dict:
    email = acc.get("email", "")
    password = acc.get("password", "")
    if not email or not password:
        log.warning(f"  跳过 {email}: 缺少密码")
        return acc

    # 先 verify 当前 cookie，若能用则不动
    if verify_refresh(acc.get("clerk_client_cookie", ""),
                      acc.get("clerk_session_id", "")):
        log.info(f"  {email}: cookie 仍可用，跳过")
        return acc

    log.info(f"{email}: cookie 失效，准备重新登录")
    # 默认 headful（登录流程需要真 Chrome，headless_shell 在 playwright install 时可能没装）
    # --show 只是给用户提示"会弹窗"，实际两种模式下都带界面
    browser = pw.chromium.launch(
        headless=False,
        args=["--disable-blink-features=AutomationControlled"],
    )
    try:
        context = browser.new_context(
            user_agent=UA,
            viewport={"width": 1366, "height": 900},
            locale="en-US",
        )
        context.add_init_script(
            "Object.defineProperty(navigator,'webdriver',{get:()=>undefined});"
        )
        info = login_one(context, email, password)
        context.close()
    finally:
        browser.close()

    if not info or not info.get("clerk_client_cookie"):
        log.error(f"  {email}: 登录失败，保留旧值")
        return acc

    # 自检
    if not verify_refresh(info["clerk_client_cookie"], info["clerk_session_id"]):
        log.error(f"  {email}: 新 cookie 自检失败")
        return acc

    # 更新字段（保留注册时的其它信息）
    acc["clerk_session_id"] = info["clerk_session_id"]
    acc["clerk_org_id"] = info.get("clerk_org_id", acc.get("clerk_org_id", ""))
    acc["clerk_client_cookie"] = info["clerk_client_cookie"]
    acc["clerk_session_cookie"] = info.get("clerk_session_cookie", "")
    acc["clerk_client_uat"] = info.get("clerk_client_uat", "")
    acc["ws_token_suffix"] = (
        f"{info.get('clerk_user_id', acc.get('clerk_user_id', ''))}:"
        f"{info.get('clerk_org_id', acc.get('clerk_org_id', ''))}"
    )
    log.info(f"  ✅ {email} cookie 已刷新")
    return acc


def main():
    ap = argparse.ArgumentParser(description="cto.new 账号 cookie 刷新器")
    ap.add_argument("--email", type=str, default="", help="只刷某一个账号，留空则全部")
    ap.add_argument("--show", action="store_true", help="显示浏览器")
    ap.add_argument("--file", type=str, default=str(ACCOUNTS_FILE))
    args = ap.parse_args()

    path = Path(args.file)
    if not path.exists():
        log.error(f"找不到 {path}")
        sys.exit(1)

    lines = path.read_text(encoding="utf-8").strip().split("\n")
    accounts = [json.loads(l) for l in lines if l.strip()]
    log.info(f"读取 {len(accounts)} 个账号")

    with sync_playwright() as pw:
        for i, acc in enumerate(accounts):
            if args.email and acc.get("email") != args.email:
                continue
            log.info(f"--- [{i+1}/{len(accounts)}] {acc.get('email')} ---")
            accounts[i] = refresh_account(pw, acc, args.show)

    # 写回
    with open(path, "w", encoding="utf-8") as f:
        for acc in accounts:
            f.write(json.dumps(acc, ensure_ascii=False) + "\n")
    log.info(f"✅ 已写回 {path}")


if __name__ == "__main__":
    main()
