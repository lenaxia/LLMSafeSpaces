#!/usr/bin/env python3
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
"""D-MODEL-LIST-ANNOTATED canary — Python SDK"""

from __future__ import annotations

import json
import sys
import os

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))

from canary import jwt_login, Runner, Config, config_from_env, wait_active
from llmsafespaces import LLMSafeSpaces


def run(r: Runner, cfg: Config) -> None:
    c = LLMSafeSpaces(cfg.api_url, api_key=jwt_login(cfg), timeout=60.0)
    ws_id = None
    cred_id = None
    try:
        ok, ws = r.assert_no_error(
            lambda: c.workspaces.create(
                name="canary-py-model-list", runtime="base", storage_size="1Gi"
            ),
            "create: no error",
        )
        if not ok:
            return
        ws_id = ws.id

        phase = wait_active(c, ws_id)
        r.assert_(phase == "Active", "reach-active", f"got {phase!r}")
        if phase != "Active":
            return

        if cfg.llm_api_key:
            cred_value = json.dumps(
                {"kind": cfg.llm_provider, "slug": "canary-py-list-anno", "apiKey": cfg.llm_api_key}
            )
            ok_cred, cred = r.assert_no_error(
                lambda: c.secrets.create(
                    name="canary-py-model-cred", type="llm-provider", value=cred_value
                ),
                "create-cred: no error",
            )
            if ok_cred and cred:
                cred_id = cred.id
                r.assert_no_error(
                    lambda: c.workspaces.set_bindings(ws_id, [cred_id]),
                    "bind-cred: no error",
                )

        ok2, models_resp = r.assert_no_error(
            lambda: c.workspaces.get_models(ws_id), "get-models: no error"
        )
        if ok2 and models_resp is not None:
            r.assert_("models" in models_resp, "get-models: has models array")
            r.assert_("currentModel" in models_resp, "get-models: has currentModel")

            models = models_resp.get("models", [])
            r.assert_(len(models) > 0, "get-models: models non-empty")

            selected_count = 0
            for m in models:
                r.assert_("id" in m, f"model: has id")
                r.assert_("name" in m, f"model: has name")
                r.assert_("tier" in m, f"model: has tier")
                r.assert_("freeTier" in m, f"model: has freeTier")
                r.assert_("selected" in m, f"model: has selected")
                if m.get("selected"):
                    selected_count += 1

            r.assert_(
                selected_count >= 1,
                "get-models: at least one selected",
                f"found {selected_count} selected",
            )

        if cfg.llm_model:
            r.assert_no_error(
                lambda: c.workspaces.set_model(ws_id, cfg.llm_model),
                "set-model: no error",
            )
            ok3, models_after = r.assert_no_error(
                lambda: c.workspaces.get_models(ws_id), "get-models-after: no error"
            )
            if ok3 and models_after is not None:
                selected_models = [
                    m for m in models_after.get("models", []) if m.get("selected")
                ]
                r.assert_(
                    len(selected_models) >= 1,
                    "after-set: model is selected",
                    f"selected: {[m.get('id') for m in selected_models]}",
                )

    finally:
        if cred_id:
            try:
                c.secrets.delete(cred_id)
            except Exception:
                pass
        if ws_id:
            try:
                c.workspaces.delete(ws_id)
            except Exception:
                pass


if __name__ == "__main__":
    r = Runner("model-list-annotated")
    cfg = config_from_env()
    run(r, cfg)
    r.print()
    sys.exit(r.exit_code())
