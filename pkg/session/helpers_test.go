// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"encoding/json"
	"reflect"
	"testing"
)

// roundTrip marshals v, unmarshals into a new zero value of the same type T,
// and asserts deep equality. Used by every contract type test.
func roundTrip[T any](t *testing.T, v T) {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got T
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v (input %s)", err, out)
	}
	if !reflect.DeepEqual(v, got) {
		t.Fatalf("round-trip mismatch\nwant: %#v\ngot:  %#v\nwire: %s", v, got, out)
	}
}
