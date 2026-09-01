// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// FaultInjectionEnvVar gates the deterministic fault-injection middleware
// (Epic 70 US-70.0, #1182). Unset/empty — or any value that parses to zero
// rules — means the feature does not exist: the router registers no
// middleware. Rule format: comma-separated COUNT:METHOD:PATH_PREFIX, e.g.
// "5:POST:/internal/v1/pod-bootstrap". The first COUNT matching requests
// fail with 500; subsequent matches pass through untouched. A malformed
// rule is a startup error, never silently ignored.
const FaultInjectionEnvVar = "LLMSAFESPACES_FAULT_INJECTION"

// FaultInjectionRule fails the first Count requests whose method equals
// Method and whose path has PathPrefix as a strings.HasPrefix prefix.
type FaultInjectionRule struct {
	Count      int64
	Method     string
	PathPrefix string
}

var faultInjectionMethods = map[string]bool{
	http.MethodConnect: true,
	http.MethodDelete:  true,
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodOptions: true,
	http.MethodPatch:   true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodTrace:   true,
}

// ParseFaultInjectionRules parses the LLMSAFESPACES_FAULT_INJECTION rule
// list. Empty segments and surrounding whitespace are trimmed; a spec that
// contains no rules parses to (nil, nil) — the feature stays absent. Any
// malformed rule fails the whole parse with an error naming the rule.
func ParseFaultInjectionRules(spec string) ([]FaultInjectionRule, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return nil, nil
	}
	var rules []FaultInjectionRule
	for _, segment := range strings.Split(trimmed, ",") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		rule, err := parseFaultInjectionRule(segment)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func parseFaultInjectionRule(segment string) (FaultInjectionRule, error) {
	parts := strings.SplitN(segment, ":", 3)
	if len(parts) != 3 {
		return FaultInjectionRule{}, fmt.Errorf("invalid %s rule %q: want COUNT:METHOD:PATH_PREFIX", FaultInjectionEnvVar, segment)
	}
	count, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	if err != nil || count < 1 {
		return FaultInjectionRule{}, fmt.Errorf("invalid %s rule %q: count must be a positive integer", FaultInjectionEnvVar, segment)
	}
	method := strings.TrimSpace(parts[1])
	if !faultInjectionMethods[method] {
		return FaultInjectionRule{}, fmt.Errorf("invalid %s rule %q: unsupported HTTP method %q", FaultInjectionEnvVar, segment, method)
	}
	prefix := strings.TrimSpace(parts[2])
	if prefix == "" {
		return FaultInjectionRule{}, fmt.Errorf("invalid %s rule %q: path prefix must not be empty", FaultInjectionEnvVar, segment)
	}
	if !strings.HasPrefix(prefix, "/") {
		return FaultInjectionRule{}, fmt.Errorf("invalid %s rule %q: path prefix must start with %q", FaultInjectionEnvVar, segment, "/")
	}
	return FaultInjectionRule{Count: count, Method: method, PathPrefix: prefix}, nil
}

type faultInjectionState struct {
	rule  FaultInjectionRule
	fired atomic.Int64
}

// FaultInjectionMiddleware returns a middleware that deterministically
// fails the first Count matching requests per rule with HTTP 500, then
// passes matching requests through. Counters are atomic; under concurrency
// exactly Count matches fail. Rules are evaluated in order and are
// independent — exhausting one rule's budget never consumes another's.
// Callers must not install the middleware when no rules are configured.
func FaultInjectionMiddleware(log interfaces.LoggerInterface, rules []FaultInjectionRule) gin.HandlerFunc {
	states := make([]faultInjectionState, len(rules))
	for i, rule := range rules {
		states[i].rule = rule
	}
	return func(c *gin.Context) {
		for i := range states {
			state := &states[i]
			if c.Request.Method != state.rule.Method || !strings.HasPrefix(c.Request.URL.Path, state.rule.PathPrefix) {
				continue
			}
			fired := state.fired.Add(1)
			if fired > state.rule.Count {
				continue
			}
			log.Warn("fault injection: request failed deliberately",
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"remaining", state.rule.Count-fired)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": fmt.Sprintf("fault injection: %s %s", state.rule.Method, state.rule.PathPrefix),
			})
			return
		}
		c.Next()
	}
}
