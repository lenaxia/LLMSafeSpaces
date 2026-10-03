# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later

"""Shared framework for Python SDK canaries.

Each canary scenario imports from this module for result tracking,
config loading, and HTTP helpers.
"""

from __future__ import annotations

import json
import os
import sys
import time
from dataclasses import dataclass, field
from typing import Any, Callable

import httpx


# ── Config ────────────────────────────────────────────────────────────────────


@dataclass
class Config:
    api_url: str
    api_key: str
    api_key_user2: str
    email: str
    password: str
    llm_provider: str
    llm_api_key: str
    llm_model: str
    bad_model: str


def config_from_env() -> Config:
    api_url = os.environ.get("LLMSAFESPACES_URL", "http://localhost:8080")
    return Config(
        api_url=api_url,
        api_key=os.environ.get("LLMSAFESPACES_API_KEY", ""),
        api_key_user2=os.environ.get("LLMSAFESPACES_API_KEY_USER2", ""),
        email=os.environ.get("LLMSAFESPACES_EMAIL", ""),
        password=os.environ.get("LLMSAFESPACES_PASSWORD", ""),
        llm_provider=os.environ.get("LLMSAFESPACES_LLM_PROVIDER", "anthropic"),
        llm_api_key=os.environ.get("LLMSAFESPACES_LLM_API_KEY", ""),
        llm_model=os.environ.get("LLMSAFESPACES_LLM_MODEL", ""),
        bad_model=os.environ.get(
            "LLMSAFESPACES_BAD_MODEL", "invalid-provider/no-such-model"
        ),
    )


# ── Result ────────────────────────────────────────────────────────────────────


@dataclass
class Check:
    name: str
    passed: bool
    detail: str = ""


@dataclass
class Result:
    scenario: str
    sdk: str
    passed: int = 0
    failed: int = 0
    duration_s: float = 0.0
    checks: list[Check] = field(default_factory=list)
    error: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "scenario": self.scenario,
            "sdk": self.sdk,
            "passed": self.passed,
            "failed": self.failed,
            "duration_s": self.duration_s,
            "checks": [
                {"name": c.name, "passed": c.passed, "detail": c.detail}
                for c in self.checks
            ],
            **({"error": self.error} if self.error else {}),
        }


class Runner:
    def __init__(self, scenario: str, sdk: str = "python-sdk"):
        self.scenario = scenario
        self.sdk = sdk
        self._start = time.monotonic()
        self._checks: list[Check] = []
        self._passed = 0
        self._failed = 0

    def assert_(self, cond: bool, name: str, detail: str = "") -> bool:
        c = Check(name=name, passed=cond, detail=detail)
        self._checks.append(c)
        if cond:
            self._passed += 1
        else:
            self._failed += 1
        return cond

    def ok(self, name: str) -> None:
        self.assert_(True, name)

    def fail(self, name: str, detail: str = "") -> None:
        self.assert_(False, name, detail)

    def assert_no_error(self, fn: Callable[[], Any], name: str) -> tuple[bool, Any]:
        """Call fn(), record pass/fail, return (ok, result)."""
        try:
            result = fn()
            self.ok(name)
            return True, result
        except Exception as e:
            self.fail(name, str(e))
            return False, None

    def assert_error(self, fn: Callable[[], Any], name: str) -> bool:
        """Call fn(), expect an exception."""
        try:
            fn()
            self.fail(name, "expected an error but got none")
            return False
        except Exception as e:
            self.assert_(True, name, str(e))
            return True

    def result(self) -> Result:
        return Result(
            scenario=self.scenario,
            sdk=self.sdk,
            passed=self._passed,
            failed=self._failed,
            duration_s=time.monotonic() - self._start,
            checks=self._checks,
        )

    def print(self) -> Result:
        res = self.result()
        print(f"=== Canary: {res.sdk} / {res.scenario} ===")
        for c in res.checks:
            mark = "PASS" if c.passed else "FAIL"
            detail = f": {c.detail}" if c.detail else ""
            print(f"  {mark} {c.name}{detail}")
        print(
            f"--- {res.passed} passed, {res.failed} failed in {res.duration_s:.2f}s ---\n"
        )
        return res

    def exit_code(self) -> int:
        return 1 if self._failed > 0 else 0


# ── HTTP helpers ──────────────────────────────────────────────────────────────


def raw_do(
    method: str,
    url: str,
    api_key: str = "",
    body: bytes | None = None,
    timeout: float = 15.0,
) -> tuple[int, bytes]:
    headers: dict[str, str] = {"Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    resp = httpx.request(method, url, headers=headers, content=body, timeout=timeout)
    return resp.status_code, resp.content


def jwt_login(cfg: "Config") -> str:
    """Log in via email/password and return the JWT bearer token.

    Some endpoints (user-credential CRUD, user-secret CRUD) require the
    session-scoped DEK that only JWT login produces (post-Epic-56). API-key
    auth alone returns 403 ``encryption key not available; re-authenticate``
    on those paths. Scenarios that exercise user-DEK-encrypted writes must
    use the JWT token returned here as ``api_key`` for subsequent calls.

    Raises ``RuntimeError`` if login fails.

    A 429 is retried once after the server-advertised ``retryAfter``
    (capped at 60s). The per-route login limiter tripped by the
    S-RATE-LIMIT scenario cannot reach a later ``jwt_login`` in the CI
    orderings (quota scenarios use the static API key, and every
    login-bearing job transition carries a 65s bucket-refill sleep;
    the sleep-less MCP transition performs no /auth/login) — the
    plausible
    source of the observed twice-on-PR-CI 429s (2026-10-03) is the
    GLOBAL limiter (advertised retryAfter ~1s), which can trip any
    request under bursty runs. The retry covers that case.
    """
    body = json.dumps({"email": cfg.email, "password": cfg.password}).encode()
    status, content = raw_do("POST", f"{cfg.api_url}/api/v1/auth/login", body=body)
    if status == 429:
        retry_after = 60.0
        try:
            retry_after = min(float(json.loads(content).get("retryAfter", 60)), 60.0)
        except (ValueError, AttributeError):
            pass
        time.sleep(retry_after + 1.0)
        status, content = raw_do("POST", f"{cfg.api_url}/api/v1/auth/login", body=body)
    if status != 200:
        raise RuntimeError(f"jwt login failed: status={status} body={content[:200]!r}")
    obj = json.loads(content)
    token = obj.get("token")
    if not token:
        raise RuntimeError(f"jwt login: no token in response: {obj}")
    return token


def has_error_field(body: bytes) -> bool:
    try:
        obj = json.loads(body)
        return isinstance(obj.get("error"), str)
    except Exception:
        return False


def has_field(body: bytes, field: str) -> bool:
    try:
        obj = json.loads(body)
        return field in obj
    except Exception:
        return False


def contains_leaked_internals(body: bytes) -> bool:
    s = body.decode("utf-8", errors="replace").lower()
    for marker in ("panic:", "runtime error:", "goroutine ", "stack trace"):
        if marker in s:
            return True
    return False


def wait_phase(client: Any, ws_id: str, target: str, timeout: float = 90) -> str:
    start = time.time()
    while time.time() - start < timeout:
        try:
            ws = client.workspaces.get(ws_id)
            if ws.phase == target:
                return ws.phase
        except Exception:
            pass
        time.sleep(3)
    try:
        ws = client.workspaces.get(ws_id)
        return ws.phase
    except Exception:
        return "unknown"


def wait_active(client: Any, ws_id: str, timeout: float = 150) -> str:
    return wait_phase(client, ws_id, "Active", timeout)


def ensure_session_with_retry(client: Any, ws_id: str, max_tries: int = 5) -> Any:
    last_err = None
    for _ in range(max_tries):
        try:
            sess = client.sessions.ensure(ws_id)
            if sess.sessionId:
                return sess
        except Exception as e:
            last_err = e
        time.sleep(5)
    raise RuntimeError(f"ensure session failed after {max_tries} tries: {last_err}")


# ── Fission HTTP handler wrapper ─────────────────────────────────────────────


def fission_handler(run_fn: Callable[[Runner, Config], None], scenario: str):
    """
    Returns a WSGI-compatible function for use as a Fission function handler.
    Fission calls this with (context, request) → response bytes.
    For local CLI execution, call run_fn directly.
    """
    try:
        from flask import Flask, jsonify  # noqa: PLC0415
    except ImportError:
        raise RuntimeError(
            "flask is required for Fission handler mode: pip install flask"
        )

    app = Flask(__name__)

    @app.route("/", methods=["GET"])
    def handle():
        run = Runner(scenario)
        cfg = config_from_env()
        run_fn(run, cfg)
        res = run.result()
        return jsonify(res.to_dict()), 200 if res.failed == 0 else 500

    return app
