package main

// user_timezone.go — the live browser-timezone channel. The frontend
// reports the user's IANA zone to the platform; the API pushes it to
// this pod's agentd on (re)connect via POST /v1/user-timezone, and the
// get_datetime tool renders user-local time from it. This is the live
// path — TZ-at-spawn would only be a snapshot and is not used.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// userTimezoneAtomic holds the platform-last-seen browser zone for this
// workspace's user ("" = never reported). Set by the control plane;
// read by get_datetime.
var userTimezoneAtomic atomic.Value

func init() {
	userTimezoneAtomic.Store("")
}

func userTimezone() string {
	return userTimezoneAtomic.Load().(string)
}

// validateTimezone reports whether name loads as an IANA zone against
// the embedded tzdata (the time/tzdata import in main — the delivery
// image is FROM scratch). It returns the input verbatim; callers that
// need a *time.Location call time.LoadLocation themselves.
func validateTimezone(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if name == "UTC" || name == "Local" {
		return name, true
	}
	loc, err := time.LoadLocation(name)
	if err != nil || loc == nil {
		return "", false
	}
	return name, true
}

// maxUserTimezoneBodyBytes bounds a /v1/user-timezone request body (the
// #1561/#1564 bounded-read convention, #1565): a timezone name is
// IANA-scale — 256 bytes is the historical bound, now enforced loudly
// (413) instead of by silent io.LimitReader truncation.
const maxUserTimezoneBodyBytes = 256

// userTimezoneHandler serves POST /v1/user-timezone — the API's live
// push when a browser session (re)connects. Control-plane gated: the
// §D1 carve-out pair (control-plane OR workspace password), identical
// to every other user-mux route the API drives.
func userTimezoneHandler(workspacePassword, agentdPassword string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkBasicAuthAny(r, agentdPassword, workspacePassword) {
			rejectUnauthorized(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Timezone string `json:"timezone"`
		}
		// #1565 (the #1561/#1564 convention): exactly one JSON
		// document (the wire's one client is this repo's
		// agentpush.PushUserTimezone — a single json.Marshal body),
		// loud diagnostics, bounded reads. The old LimitReader hid
		// bytes past the cap entirely: a corrupted push was silently
		// half-honored (first document stored, remainder unseen).
		if err := decodeOneDocument(http.MaxBytesReader(w, r.Body, maxUserTimezoneBodyBytes), &body); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, fmt.Sprintf("request body exceeds the %d-byte cap", maxUserTimezoneBodyBytes), http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
			return
		}
		canonical, ok := validateTimezone(body.Timezone)
		if !ok {
			http.Error(w, "invalid or unknown timezone", http.StatusBadRequest)
			return
		}
		userTimezoneAtomic.Store(canonical)
		if log != nil {
			log.Debug("user timezone updated", zap.String("timezone", canonical))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "timezone": canonical})
	}
}
