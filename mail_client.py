#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
mail_client.py —— 浏览器驱动的临时邮箱客户端
=============================================
唯一实现：**浏览器直驱 mail.chatgpt.org.uk (GPTMail)**

为什么不走 API：
* 公共 API key 的每日额度容易被打满（sk-m2o02lmd4hN0 已耗尽）
* 申请私有 key 需要给 chatgpt.org.uk 添加自有域名 MX 记录，流程重
* 而 **浏览器页面本身不需要 key**：
  - 访问 `https://mail.chatgpt.org.uk/` 会 302 分配一个随机邮箱到 URL
  - 页面内部通过 WebSocket 实时推送新邮件（页面上有 "Live connected" 标记）
  - DOM 里的 `ul.email-list` 会更新，body 文本直接可以正则拿验证码

协议：
* 分配邮箱  → 打开 `https://mail.chatgpt.org.uk/`，等 URL 变为 `/{email}`
* 等验证码  → 切到 mail tab，轮询 body 文本，匹配 Clerk 邮件主题
              "XXXXXX is your verification code"

设计要点：
* 本客户端强依赖一个 SeleniumBase SB 实例，必须在 SB 启动后才能创建
* 所有方法线程内串行调用，不做并发
* 使用独立 tab 做邮箱，不干扰注册流程用的主 tab
"""
from __future__ import annotations
import os
import re
import time
import logging
from typing import Optional

log = logging.getLogger(__name__)

# ============================================================
# 常量
# ============================================================
MAIL_HOME = "https://mail.chatgpt.org.uk/"

# Clerk 验证码邮件主题固定格式："XXXXXX is your verification code"
CLERK_CODE_BODY_RE = re.compile(
    r"\b(\d{6})\s+is your verification code",
    re.IGNORECASE,
)
# 兜底：通用 6 位数字
GENERIC_CODE_RE = re.compile(r"\b(\d{6})\b")


# ============================================================
# BrowserMailClient —— 浏览器直驱
# ============================================================
class BrowserMailClient:
    """基于 SeleniumBase SB 实例驱动 mail.chatgpt.org.uk。

    使用方式：
        mail = BrowserMailClient(sb)
        email = mail.generate()          # 新 tab 打开 mail，URL 分配 email
        # ... 注册流程在原 tab 提交后 ...
        code = mail.wait_code(email)     # 切回 mail tab 等邮件
    """

    def __init__(self, sb, page_load_wait: float = 3.0):
        self.sb = sb
        self.page_load_wait = page_load_wait
        self._email: Optional[str] = None
        self._mail_tab_handle: Optional[str] = None
        self._register_tab_handle: Optional[str] = None

    # -------- 分配邮箱（串行版本，向后兼容）--------
    def generate(
        self,
        prefix: Optional[str] = None,
        domain: Optional[str] = None,
    ) -> str:
        """同步版本：打开 mail tab → 等 URL → 切回。保持向后兼容。

        新代码建议用 `start_background()` + `collect_email()` 实现并行加载。
        """
        self.start_background()
        return self.collect_email()

    # -------- 并行模式：先启动后台加载，再收获邮箱 --------
    def start_background(self) -> None:
        """打开 mail tab 但**不等 URL**，立即返回，让 mail 页在后台加载。

        配合 `collect_email()` 使用：
            mail.start_background()        # t=0，立即返回
            sb.uc_open_with_reconnect(...) # t=0~3s，signup tab 加载（主线程 block）
            email = mail.collect_email()   # t=3~4s，mail 已 302 完，秒读 URL
        """
        import json as _json
        # 1) 记录注册 tab handle（此时只有 1 个 tab，就是主 tab）
        try:
            self._register_tab_handle = self.sb.driver.current_window_handle
        except Exception:
            try:
                handles = self.sb.driver.window_handles
                self._register_tab_handle = handles[0] if handles else None
            except Exception:
                self._register_tab_handle = None
        log.info("register tab handle: %s", self._register_tab_handle)

        # 2) window.open 打开 mail tab 并立即切过去做一次"启动确认"后就切回
        #    必须切过去才能让 mail 页开始加载（Chrome 后台 tab 会延迟执行）
        self.sb.execute_script(
            f"window.open({_json.dumps(MAIL_HOME)}, '_blank')"
        )
        time.sleep(0.3)

        # 3) 切到最新 tab 记录 handle
        switched = False
        try:
            self.sb.switch_to_newest_window()
            switched = True
        except Exception as e:
            log.warning("switch_to_newest_window 失败: %s", e)
            try:
                handles = self.sb.driver.window_handles
                if len(handles) >= 2:
                    self.sb.switch_to_window(handles[-1])
                    switched = True
            except Exception as e2:
                log.error("手动 switch_to_window 也失败: %s", e2)
        if not switched:
            raise RuntimeError("无法切到 mail tab")

        try:
            self._mail_tab_handle = self.sb.driver.current_window_handle
        except Exception:
            self._mail_tab_handle = None
        log.info("mail tab handle: %s (后台加载中)", self._mail_tab_handle)

        # 4) 立即切回主 tab，让 mail 页在后台继续加载
        self._switch_to_register_tab()

    def collect_email(self) -> str:
        """从 mail tab 读 URL 里的邮箱。配合 start_background() 使用。

        假设 mail tab 已经加载了一段时间（3s+），此时 URL 应该已经 302 完。
        最多等 8s，超时抛异常。
        """
        if not self._mail_tab_handle:
            raise RuntimeError("collect_email 调用前必须先 start_background()")

        # 切到 mail tab
        if not self._switch_to_mail_tab():
            raise RuntimeError("无法切到 mail tab")

        # 轮询 URL（服务端 302 很快）
        email = None
        for _ in range(27):  # 最多 ~8s
            try:
                url = self.sb.get_current_url()
                if url and url != "about:blank":
                    mo = re.search(
                        r"mail\.chatgpt\.org\.uk/([A-Za-z0-9._+-]+@[A-Za-z0-9.-]+\.[A-Za-z]+)",
                        url,
                    )
                    if mo:
                        email = mo.group(1)
                        break
            except Exception as e:
                log.debug("get_current_url 异常: %s", e)
            time.sleep(0.3)

        if not email:
            url_dump = "?"
            try:
                url_dump = self.sb.get_current_url()
            except Exception:
                pass
            self._switch_to_register_tab()
            raise RuntimeError(f"mail 页 URL 未出现邮箱: {url_dump}")

        # 复用场景防污染
        if self._email and email == self._email:
            log.info("检测到和上次相同的邮箱（localStorage 污染）→ 清 storage 重刷")
            email = self._force_refresh_for_new_email(fallback=email)

        self._email = email
        domain_part = email.split("@", 1)[1] if "@" in email else "?"
        log.info("✅ 分配随机邮箱: %s  (域名: %s, 并行后台加载)",
                 email, domain_part)

        # 切回主 tab
        self._switch_to_register_tab()
        return self._email

    def _force_refresh_for_new_email(self, fallback: str) -> str:
        """仅在复用场景被调用：清 localStorage 后 driver.refresh() 让服务端重分配。

        不走 window.generateNewEmail()，原因：
        * SB UC Mode 下 execute_script 走 CDP evaluate（isolated world），
          拿不到 main world 的 window.generateNewEmail 函数；实测始终返回 false
        * 刷新后服务端会重新 302 到新随机邮箱，比等 JS bundle 加载快得多
        """
        try:
            self.sb.execute_script(
                "try{window.localStorage.clear();window.sessionStorage.clear();}catch(e){}"
            )
        except Exception as e:
            log.debug("清 storage 失败: %s", e)

        try:
            # SB 没有直接 refresh 方法，走 driver.refresh
            self.sb.driver.refresh()
        except Exception as e:
            log.warning("refresh 失败: %s → 用 fallback", e)
            return fallback

        # 快速轮询新邮箱
        for _ in range(20):  # 最多 6s
            time.sleep(0.3)
            try:
                url = self.sb.get_current_url()
                if url:
                    mo = re.search(
                        r"mail\.chatgpt\.org\.uk/([A-Za-z0-9._+-]+@[A-Za-z0-9.-]+\.[A-Za-z]+)",
                        url,
                    )
                    if mo and mo.group(1) != fallback:
                        return mo.group(1)
            except Exception:
                pass
        log.warning("refresh 后仍拿不到新邮箱 → 用 fallback")
        return fallback

    # -------- 等验证码 --------
    def wait_code(
        self,
        email: str,
        known_ids: Optional[set] = None,
        timeout: int = 120,
        interval: int = 1,  # 从 3 缩到 1；Clerk 一般 5-15s 发到，快速轮询早点拿到
    ) -> str:
        """切到 mail tab，轮询 body 文本，返回第一个 Clerk 验证码。

        优先匹配 "XXXXXX is your verification code"，兜底匹配 body 里第一个 6 位数字。
        """
        deadline = time.time() + timeout
        last_len = -1
        last_switch_fail = 0
        while time.time() < deadline:
            # 切到 mail tab
            switched = self._switch_to_mail_tab()
            if not switched:
                last_switch_fail += 1
                if last_switch_fail >= 5:
                    raise RuntimeError(
                        "mail tab 切换连续失败 5 次，无法继续等待邮件"
                    )
                time.sleep(1)
                continue
            last_switch_fail = 0

            try:
                body = self._get_body_text()
                if body and len(body) != last_len:
                    log.debug(
                        "mail body 长度变化: %d → %d",
                        last_len, len(body),
                    )
                    last_len = len(body)

                # 优先：Clerk 固定主题格式
                mo = CLERK_CODE_BODY_RE.search(body or "")
                if mo:
                    code = mo.group(1)
                    log.info("从 mail DOM 提取到 Clerk 验证码: %s", code)
                    # 切回注册 tab
                    self._switch_to_register_tab()
                    return code

                # 兜底：检测"收到新邮件"的 DOM 变化
                if body and "your inbox is empty" not in body.lower():
                    mo2 = GENERIC_CODE_RE.search(body)
                    if mo2:
                        code = mo2.group(1)
                        log.info("从 mail DOM 正文提取到 6 位: %s（兜底）", code)
                        self._switch_to_register_tab()
                        return code
            except Exception as e:
                log.warning("mail 轮询异常: %s", e)

            time.sleep(interval)

        # 超时：切回原 tab 再抛
        try:
            self._switch_to_register_tab()
        except Exception:
            pass
        raise TimeoutError(
            f"mail.chatgpt.org.uk 等验证码超时（{timeout}s, email={email}）"
        )

    # -------- 列表 / 已知 id（兼容旧接口）--------
    def list(self, email: str) -> list:
        """返回 []（本实现不维护邮件列表，直接走 DOM）。"""
        return []

    def list_ids(self, email: str) -> set:
        return set()

    # -------- 内部工具：tab 切换（高鲁棒性版本）--------
    # GPTMail 会蹦 Google AdSense popunder（#google_vignette），可能导致
    # sb.switch_to_window 在某些边缘态失败。全部兜底到 sb.driver.switch_to.window。
    def _switch_to_mail_tab(self) -> bool:
        return self._switch_to_handle(
            self._mail_tab_handle,
            fallback_idx=-1,
            label="mail",
        )

    def _switch_to_register_tab(self) -> bool:
        return self._switch_to_handle(
            self._register_tab_handle,
            fallback_idx=0,
            label="register",
        )

    def _switch_to_handle(self, handle: Optional[str],
                          fallback_idx: int, label: str) -> bool:
        if not handle:
            log.warning("switch_to(%s): handle 为空", label)
            return False
        # 优先：SB API
        try:
            self.sb.switch_to_window(handle)
            return True
        except Exception as e:
            log.debug("sb.switch_to_window(%s) 失败: %s", label, e)
        # 退 1：直接用 selenium driver API
        try:
            self.sb.driver.switch_to.window(handle)
            return True
        except Exception as e:
            log.debug("driver.switch_to.window(%s) 失败: %s", label, e)
        # 退 2：按 index 切
        try:
            handles = self.sb.driver.window_handles
            idx = fallback_idx if fallback_idx >= 0 else len(handles) - 1
            if 0 <= idx < len(handles):
                self.sb.driver.switch_to.window(handles[idx])
                return True
        except Exception as e:
            log.debug("driver.switch_to.window(idx=%d) 失败: %s",
                      fallback_idx, e)
        log.error("switch_to_handle(%s) 三路全失败", label)
        return False

    def _get_body_text(self) -> str:
        """取 body 全文。UC Mode 下 sb.get_text('body') 走 selenium，
        失败时退回 CDP execute_script。"""
        try:
            return self.sb.get_text("body") or ""
        except Exception:
            try:
                return self.sb.execute_script("document.body.innerText || ''") or ""
            except Exception:
                return ""


# ============================================================
# 对外统一入口 MailClient
# ============================================================
class MailClient:
    """统一邮箱客户端，必须传入 SeleniumBase sb 实例。"""

    def __init__(self, sb=None, provider: Optional[str] = None, **kwargs):
        if sb is None:
            raise ValueError(
                "MailClient 需要传入 SeleniumBase sb 实例（用 "
                "MailClient(sb=sb) 创建）"
            )
        self._impl = BrowserMailClient(sb, **kwargs)
        self.provider = "gptmail-browser"
        log.info("MailClient 已选用: %s", self.provider)

    def generate(self, prefix: Optional[str] = None, domain: Optional[str] = None) -> str:
        return self._impl.generate(prefix=prefix, domain=domain)

    # -------- 并行模式 --------
    def start_background(self) -> None:
        """打开 mail tab 但不等 URL，立即返回（让 mail 在后台加载）"""
        return self._impl.start_background()

    def collect_email(self) -> str:
        """从后台加载的 mail tab 里读出邮箱"""
        return self._impl.collect_email()

    def list(self, email: str) -> list:
        return self._impl.list(email)

    def list_ids(self, email: str) -> set:
        return self._impl.list_ids(email)

    def wait_code(
        self,
        email: str,
        known_ids: Optional[set] = None,
        timeout: int = 120,
        interval: int = 1,
    ) -> str:
        return self._impl.wait_code(email, known_ids, timeout=timeout, interval=interval)


# ============================================================
# 自检：打开 mail.chatgpt.org.uk 拿一个邮箱，不进注册流程
# ============================================================
if __name__ == "__main__":
    import sys
    logging.basicConfig(
        level=logging.INFO,
        format="[%(asctime)s] %(levelname)s - %(message)s",
        datefmt="%H:%M:%S",
    )
    from seleniumbase import SB

    with SB(uc=True, test=True, locale="en") as sb:
        mail = MailClient(sb=sb)
        email = mail.generate()
        print(f"分配邮箱: {email}")
        print(f"mail tab handle: {mail._impl._mail_tab_handle}")

        if "--wait" in sys.argv:
            print(f"等验证码 120s（从外部发一封含验证码的邮件到 {email}）...")
            try:
                code = mail.wait_code(email, timeout=120, interval=3)
                print(f"验证码: {code}")
            except TimeoutError as e:
                print(f"超时: {e}")
