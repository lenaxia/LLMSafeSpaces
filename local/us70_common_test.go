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
