#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""S-SECRET-CRUD canary — Python SDK"""

from __future__ import annotations

import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import Runner, Config, config_from_env, jwt_login
from llmsafespaces import LLMSafeSpaces, NotFoundError, ConflictError


def run(run: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=20.0)
    secret_id = None

    try:
        # P1: Create
        ok, secret = run.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-secret", type="env-secret", value="canary-val", metadata={"var_name": "CANARY_PY_VAR"}
            ),
            "create: no error",
        )
        if not ok:
            return
        run.assert_(secret.id != "", "create: id non-empty")
        run.assert_(secret.name == "canary-py-secret", "create: name", secret.name)
        run.assert_(secret.type == "env-secret", "create: type", secret.type)
        secret_id = secret.id

        # P2: List
        ok2, lst = run.assert_no_error(lambda: c.secrets.list(), "list: no error")
        if ok2:
            found = any(s.id == secret_id for s in lst)
            run.assert_(found, "list: secret present")

        # P3: Get
        ok3, got = run.assert_no_error(
            lambda: c.secrets.get(secret_id), "get: no error"
        )
        if ok3:
            run.assert_(got.name == "canary-py-secret", "get: name", got.name)

        # P4: Update
        run.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-secret2", type="env-secret", value="updated-val", metadata={"var_name": "CANARY_PY_VAR"}
            ),
            "update-via-create: no error",
        )
        # Clean up second secret
        lst2 = c.secrets.list()
        for s in lst2:
            if s.name == "canary-py-secret2":
                c.secrets.delete(s.id)
                break

        # P5: Delete
        run.assert_no_error(lambda: c.secrets.delete(secret_id), "delete: no error")
        secret_id = None

        # P6: Re-create with same name after delete
        ok4, secret2 = run.assert_no_error(
            lambda: c.secrets.create(
                name="canary-py-secret", type="env-secret", value="v2", metadata={"var_name": "CANARY_PY_VAR"}
            ),
            "re-create-after-delete: no error",
        )
        if ok4:
            run.assert_(secret2.id != secret.id, "re-create-after-delete: new id")
            c.secrets.delete(secret2.id)

    finally:
        if secret_id:
            try:
                c.secrets.delete(secret_id)
            except Exception:
                pass

    # N1: Get nonexistent
    run.assert_error(
        lambda: c.secrets.get("00000000-0000-0000-0000-000000000099"),
        "get-nonexistent: NotFoundError",
    )

    # N2: Invalid name (uppercase)
    run.assert_error(
        lambda: c.secrets.create(name="My-Secret-UPPER", type="env-secret", value="x", metadata={"var_name": "CANARY_PY_VAR"}),
        "create-invalid-name: error",
    )

    # N3: Empty name
    run.assert_error(
        lambda: c.secrets.create(name="", type="env-secret", value="x", metadata={"var_name": "CANARY_PY_VAR"}),
        "create-empty-name: error",
    )

    # N4: Duplicate name
    s1, s2 = None, None
    try:
        s1 = c.secrets.create(name="canary-py-dup", type="env-secret", value="v1", metadata={"var_name": "CANARY_PY_VAR"})
        run.assert_error(
            lambda: c.secrets.create(
                name="canary-py-dup", type="env-secret", value="v2",
                metadata={"var_name": "CANARY_PY_VAR"},
            ),
            "create-duplicate: ConflictError",
        )
    except Exception as e:
        run.fail("duplicate-setup: unexpected", str(e))
    finally:
        if s1:
            try:
                c.secrets.delete(s1.id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("secret-crud")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
