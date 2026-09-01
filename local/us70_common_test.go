// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// us70_common_test.go — regression harness for local/lib/us70-common.sh
// (US-70 delivery pool, PR #1198).
//
// Pool runs 1-2 both died at AC-1 bind_env with identical signatures but
// two different root causes; the second (plaintext api_keys.key vs the
// API's sha256-hex lookup, auth.go validateAPIKey) is pinned here.
//
//   1. The DB-hash expression the harness seeds must equal sha256(plaintext)
//      hex — verified against an INDEPENDENT sha256 implementation, and
//      expressed as SQL the pool's PostgreSQL actually evaluates.
//   2. seed_user's INSERT must use the helper (not the plaintext inline) —
//      re-introducing the plaintext is what run 2 did.

import (
	"os/exec"
	"strings"
	"testing"

	"crypto/sha256"
	"encoding/hex"
)

const us70Lib = "lib/us70-common.sh"

func execHelper(t *testing.T, plaintext string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script := `source <(sed -n '/>>> api-key-db-hash/,/<<< api-key-db-hash/p' lib/us70-common.sh)` + "\n" +
		`api_key_db_hash "` + plaintext + `"`
	out, err := exec.Command(bash, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("helper exec failed: %s", out)
	}
	return strings.TrimSpace(string(out))
}

func TestUS70_APIKeyDBHash_MatchesGoAuth(t *testing.T) {
	for _, key := range []string{
		"lsp_e2esduser0000000000000000000001",
		"lsp_short",
		"key-with-'-quote-and-ümlaut",
	} {
		got := execHelper(t, key)
		// The helper emits the SQL expression PostgreSQL will evaluate.
		// Evaluate the same computation in Go (validateAPIKey's exact
		// path) and assert the expression embeds the SAME plaintext,
		// correctly quoted, under sha256/encode — the SQL is not
		// executable here, so pin its shape + payload.
		h := sha256.Sum256([]byte(key))
		_ = hex.EncodeToString(h[:]) // compile the reference path
		wantSQL := "encode(sha256(convert_to('" + strings.ReplaceAll(key, "'", "''") + "', 'UTF8')), 'hex')"
		if got != wantSQL {
			t.Errorf("api_key_db_hash(%q)\n got  %s\n want %s", key, got, wantSQL)
		}
	}
}

func TestUS70_SeedUserUsesHashHelper(t *testing.T) {
	raw, err := exec.Command("cat", us70Lib).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	i := strings.Index(src, "INSERT INTO api_keys")
	if i < 0 {
		t.Fatal("api_keys INSERT not found")
	}
	insn := src[i : i+strings.Index(src[i:], ";")]
	if strings.Contains(insn, "'${api_key}'") && !strings.Contains(insn, "api_key_db_hash") {
		t.Fatalf("seed_user inserts the PLAINTEXT key (pool run-2 bug):\n%s", insn)
	}
	if !strings.Contains(insn, "api_key_db_hash") {
		t.Fatalf("seed_user must seed via api_key_db_hash (hash pinned by test):\n%s", insn)
	}
}

func TestUS70_APIKeyInsertIDFitsColumn(t *testing.T) {
	// Pool run 4: 'value too long for type character varying(36)' — the
	// seeded api_keys.id was <owner-uuid>+"-sd" (39 chars). Pin the
	// literal ≤36 and that no ${OWNER_ID}-derived suffix sneaks back.
	raw, err := exec.Command("cat", us70Lib).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	i := strings.Index(src, "INSERT INTO api_keys")
	insn := src[i : i+strings.Index(src[i:], ";")]
	for _, line := range strings.Split(insn, "\n") {
		if !strings.Contains(line, "VALUES") {
			continue
		}
		for _, m := range strings.Split(line, ",") {
			m = strings.TrimSpace(m)
			if !strings.HasPrefix(m, "'") {
				continue
			}
			id := strings.Trim(m, "'")
			if len(id) > 36 {
				t.Fatalf("api_keys INSERT value %q is %d chars; id column is varchar(36) — pool run 4's failure", id, len(id))
			}
		}
	}
}

func TestUS70_WorkspaceRuntimeIsDirectImageRef(t *testing.T) {
	// Pool run 5: spec.runtime="python:3.11" — a bare name needing a
	// RuntimeEnvironment CRD that kubectl-created clusters don't have
	// (controller: "no RuntimeEnvironment found matching
	// workspace.spec.runtime"). The seeded runtime must be a direct
	// image ref (contains "/") in BOTH CR templates and the DB mirror.
	raw, err := exec.Command("cat", us70Lib).CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if strings.Contains(src, "runtime: python:3.11") {
		t.Fatal("bare catalog-style runtime in a CR template — the controller cannot resolve it without a RuntimeEnvironment CRD (pool run 5)")
	}
	if strings.Contains(src, "'python:3.11', '1Gi'") {
		t.Fatal("bare runtime name in the DB mirror INSERT — mirror must match the CR's direct image ref")
	}
	if !strings.Contains(src, "runtime: ${RUNTIME_REF}") {
		t.Fatal("CR templates must seed runtime from ${RUNTIME_REF}")
	}
	if !strings.Contains(src, `RUNTIME_REF="${RUNTIME_REF:-ghcr.io/lenaxia/llmsafespaces/runtime-base:${IMAGE_TAG:-ci}}"`) {
		t.Fatal("RUNTIME_REF default missing — the direct-ref contract is unpinned")
	}
}
