#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
api_mail_client.py —— GPTMail 纯 API 邮箱客户端（无需浏览器）
=============================================================
通过 GPTMail REST API 直接操作邮箱，完全不需要 SeleniumBase 浏览器 tab，
彻底消除 mail tab 广告弹窗劫持焦点的问题。

API 流程：
  1) GET / → 获取 gm_sid session cookie
  2) POST /api/inbox-token → 获取初始 auth token
  3) GET /api/generate-email → 生成随机邮箱 + 获取绑定邮箱的 token
  4) GET /api/emails?email=xxx → 轮询邮件列表
  5) GET /api/email/{id} → 获取邮件详情（含验证码）

优势：
  * 不需要浏览器 tab，不会劫持焦点
  * 速度更快（纯 HTTP 请求，无页面渲染开销）
  * 更稳定（不受 AdSense 广告弹窗干扰）
  * 可独立于 SeleniumBase 使用
"""
from __future__ import annotations

import re
import time
import json
import logging
from typing import Optional

import requests

log = logging.getLogger(__name__)

# ============================================================
# 常量
# ============================================================
MAIL_BASE = "https://mail.chatgpt.org.uk"
UA = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.0.0 Safari/537.36"
)

# Clerk 验证码邮件正则
CLERK_CODE_RE = re.compile(r"\b(\d{6})\s+is your verification code", re.IGNORECASE)
GENERIC_CODE_RE = re.compile(r"\b(\d{6})\b")


class APIMailClient:
    """GPTMail 纯 API 邮箱客户端。

    使用方式：
        mail = APIMailClient()
        email = mail.generate()           # 生成随机邮箱
        # ... 注册流程 ...
        code = mail.wait_code(email)      # 轮询邮件获取验证码
    """

    def __init__(self):
        self._session = requests.Session()
        self._session.headers.update({
            "User-Agent": UA,
            "Origin": MAIL_BASE,
            "Referer": f"{MAIL_BASE}/",
        })
        self._token: str = ""
        self._email: Optional[str] = None

    # -------- 初始化 session --------
    def _init_session(self) -> str:
        """访问主页获取 gm_sid cookie + 初始 token。"""
        r = self._session.get(MAIL_BASE + "/", timeout=15)
        if r.status_code != 200:
            raise RuntimeError(f"mail homepage HTTP {r.status_code}")

        # 获取初始 inbox token
        r2 = self._session.post(
            MAIL_BASE + "/api/inbox-token",
            json={"email": ""},
            headers={"Content-Type": "application/json"},
            timeout=10,
        )
        if r2.status_code != 200:
            raise RuntimeError(f"inbox-token HTTP {r2.status_code}: {r2.text[:200]}")
        data = r2.json()
        token = data.get("auth", {}).get("token", "")
        if not token:
            raise RuntimeError(f"inbox-token 未返回 token: {r2.text[:200]}")
        self._token = token
        return token

    # -------- 生成邮箱 --------
    def generate(self, prefix: Optional[str] = None, domain: Optional[str] = None) -> str:
        """生成一个新的随机临时邮箱地址。"""
        if not self._token:
            self._init_session()

        headers = {"x-inbox-token": self._token}
        if prefix:
            # 带前缀生成
            body = {"prefix": prefix}
            if domain:
                body["domain"] = domain
            r = self._session.post(
                MAIL_BASE + "/api/generate-email",
                json=body,
                headers=headers,
                timeout=10,
            )
        else:
            r = self._session.get(
                MAIL_BASE + "/api/generate-email",
                headers=headers,
                timeout=10,
            )

        if r.status_code == 429:
            raise RuntimeError("GPTMail 请求频率超限（429）")
        if r.status_code != 200:
            raise RuntimeError(f"generate-email HTTP {r.status_code}: {r.text[:200]}")

        data = r.json()
        if not data.get("success"):
            raise RuntimeError(f"generate-email 失败: {data.get('error', 'unknown')}")

        email = data.get("data", {}).get("email", "") or data.get("email", "")
        if not email:
            raise RuntimeError(f"generate-email 未返回邮箱: {r.text[:200]}")

        # generate 返回的 auth.token 是绑定该邮箱的新 token
        new_token = data.get("auth", {}).get("token", "")
        if new_token:
            self._token = new_token

        self._email = email
        domain_part = email.split("@", 1)[1] if "@" in email else "?"
        log.info("✅ API 分配邮箱: %s (域名: %s)", email, domain_part)
        return email

    # -------- 并行模式（兼容接口，API 版无需并行）--------
    def start_background(self) -> None:
        """API 版无需后台加载，直接 generate。"""
        self.generate()

    def collect_email(self) -> str:
        """返回已生成的邮箱。"""
        if not self._email:
            raise RuntimeError("collect_email 前必须先 start_background/generate")
        return self._email

    # -------- 获取邮件列表 --------
    def _fetch_emails(self, email: str) -> list:
        """调用 API 获取邮件列表。"""
        if not self._token:
            raise RuntimeError("token 未初始化")
        r = self._session.get(
            MAIL_BASE + f"/api/emails?email={email}",
            headers={"x-inbox-token": self._token},
            timeout=10,
        )
        if r.status_code == 403:
            # token 过期，尝试刷新
            log.debug("邮件列表 403，尝试刷新 token")
            self._refresh_token(email)
            r = self._session.get(
                MAIL_BASE + f"/api/emails?email={email}",
                headers={"x-inbox-token": self._token},
                timeout=10,
            )
        if r.status_code != 200:
            log.warning("获取邮件列表 HTTP %d: %s", r.status_code, r.text[:200])
            return []
        data = r.json()
        # 更新 token（API 会在响应里附带刷新后的 token）
        new_token = data.get("auth", {}).get("token", "")
        if new_token:
            self._token = new_token
        return data.get("data", {}).get("emails", [])

    # -------- 获取邮件详情 --------
    def _fetch_email_detail(self, email_id: str) -> dict:
        """获取单封邮件详情。"""
        r = self._session.get(
            MAIL_BASE + f"/api/email/{email_id}",
            headers={"x-inbox-token": self._token},
            timeout=10,
        )
        if r.status_code != 200:
            return {}
        data = r.json()
        new_token = data.get("auth", {}).get("token", "")
        if new_token:
            self._token = new_token
        return data.get("data", {})

    # -------- 刷新 token --------
    def _refresh_token(self, email: str) -> None:
        """刷新 inbox token。"""
        r = self._session.post(
            MAIL_BASE + "/api/inbox-token",
            json={"email": email},
            headers={"Content-Type": "application/json"},
            timeout=10,
        )
        if r.status_code == 200:
            data = r.json()
            new_token = data.get("auth", {}).get("token", "")
            if new_token:
                self._token = new_token
                log.debug("token 刷新成功")
            else:
                log.warning("token 刷新未返回新 token")
        else:
            log.warning("token 刷新失败 HTTP %d", r.status_code)

    # -------- 等验证码 --------
    def wait_code(
        self,
        email: str,
        known_ids: Optional[set] = None,
        timeout: int = 120,
        interval: int = 2,
    ) -> str:
        """轮询邮件 API，返回 Clerk 验证码。

        优先匹配 "XXXXXX is your verification code"，
        兜底匹配邮件正文中的 6 位数字。
        """
        known = known_ids or set()
        deadline = time.time() + timeout
        last_count = -1

        while time.time() < deadline:
            try:
                emails = self._fetch_emails(email)
                if len(emails) != last_count:
                    log.debug("邮件数: %d → %d", last_count, len(emails))
                    last_count = len(emails)

                for em in emails:
                    eid = em.get("id", "")
                    if eid in known:
                        continue

                    subject = em.get("subject", "")
                    # 优先：Clerk 固定主题
                    mo = CLERK_CODE_RE.search(subject)
                    if mo:
                        code = mo.group(1)
                        log.info("从邮件主题提取 Clerk 验证码: %s", code)
                        return code

                    # 获取邮件详情
                    detail = self._fetch_email_detail(eid)
                    content = detail.get("content", "")
                    if not content:
                        # 列表里的 preview 也可用
                        content = em.get("preview", "") or em.get("content", "")

                    # 去除 HTML 标签
                    clean = re.sub(r"<[^>]*>", " ", content)
                    clean = re.sub(r"\s+", " ", clean)

                    # 优先：Clerk 格式
                    mo = CLERK_CODE_RE.search(clean)
                    if mo:
                        code = mo.group(1)
                        log.info("从邮件正文提取 Clerk 验证码: %s", code)
                        return code

                    # 兜底：6 位数字
                    mo2 = GENERIC_CODE_RE.search(clean)
                    if mo2:
                        code = mo2.group(1)
                        log.info("从邮件正文提取 6 位验证码(兜底): %s", code)
                        return code

            except Exception as e:
                log.warning("邮件轮询异常: %s", e)

            time.sleep(interval)

        raise TimeoutError(
            f"GPTMail API 等验证码超时（{timeout}s, email={email}）"
        )

    # -------- 兼容接口 --------
    def list(self, email: str) -> list:
        """返回邮件列表。"""
        return self._fetch_emails(email)

    def list_ids(self, email: str) -> set:
        """返回已知邮件 ID 集合。"""
        emails = self._fetch_emails(email)
        return {em.get("id", "") for em in emails if em.get("id")}


# ============================================================
# 自检
# ============================================================
if __name__ == "__main__":
    import sys
    logging.basicConfig(
        level=logging.DEBUG,
        format="[%(asctime)s] %(levelname)s - %(message)s",
        datefmt="%H:%M:%S",
    )
    mail = APIMailClient()
    email = mail.generate()
    print(f"分配邮箱: {email}")

    if "--wait" in sys.argv:
        print(f"等验证码 120s（从外部发一封含验证码的邮件到 {email}）...")
        try:
            code = mail.wait_code(email, timeout=120, interval=2)
            print(f"验证码: {code}")
        except TimeoutError as e:
            print(f"超时: {e}")
