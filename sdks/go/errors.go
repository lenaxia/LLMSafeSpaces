// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package llmsafespaces

import "fmt"

// APIError represents an error response from the LLMSafeSpaces API.
type APIError struct {
	Status     int
	Message    string
	Reason     string // structured reason for 503s (not_ready, agent_unreachable, agent_restarting)
	RetryAfter int    // seconds to wait before retrying (for 429/503)
	Phase      string // current workspace phase on upload 409s (Epic 67 D5)
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llmsafespaces: %d %s", e.Status, e.Message)
}

// IsNotFound returns true if the error is a 404.
func IsNotFound(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == 404
	}
	return false
}

// IsAuth returns true if the error is a 401 or 403.
func IsAuth(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == 401 || e.Status == 403
	}
	return false
}

// IsConflict returns true if the error is a 409.
func IsConflict(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == 409
	}
	return false
}

// IsRateLimit returns true if the error is a 429.
func IsRateLimit(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == 429
	}
	return false
}

// IsServiceUnavailable returns true if the error is a 503 — the workspace
// exists but the agent is unreachable, restarting, or not yet booted.
func IsServiceUnavailable(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.Status == 503
	}
	return false
}
