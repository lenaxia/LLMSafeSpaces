// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
	canary "github.com/lenaxia/llmsafespaces/sdks/canary/go"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	run := canary.NewRunner("session-limit", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(r.Context(), 480*time.Second)
	defer cancel()
	runSessionLimit(ctx, run, cfg)
	run.WriteHTTP(w)
}

func main() {
	run := canary.NewRunner("session-limit", "go-sdk")
	cfg := canary.ConfigFromEnv()
	ctx, cancel := context.WithTimeout(context.Background(), 480*time.Second)
	defer cancel()
	runSessionLimit(ctx, run, cfg)
	res := run.Print()
	if res.Failed > 0 {
		os.Exit(1)
	}
}

func runSessionLimit(ctx context.Context, run *canary.Runner, cfg canary.Config) {
	if cfg.LLMAPIKey == "" {
		run.OK("skipped: no LLM key")
		return
	}

	c := cfg.Client()

	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "canary-session-limit", Runtime: "base", StorageSize: "1Gi",
	})
	if !run.AssertNoError(err, "create-ws") {
		return
	}
	wsID := ws.ID
	defer func() { _ = c.Workspaces.Delete(context.Background(), wsID) }()

	phase := canary.WaitActive(ctx, c, wsID)
	run.Assert(phase == "Active", "ws-active", fmt.Sprintf("got %q", phase))
	if phase != "Active" {
		return
	}

	activeInfo, err := c.Sessions.GetActive(ctx, wsID)
	if !run.AssertNoError(err, "get-active-info") {
		return
	}
	maxActive := activeInfo.MaxActive
	run.Assert(maxActive > 0, "max-active-positive", fmt.Sprintf("got %d", maxActive))

	var sessionIDs []string
	for i := 0; i < maxActive+2; i++ {
		sess, err := c.Sessions.Ensure(ctx, wsID)
		if err != nil || sess.SessionID == "" {
			break
		}
		sessionIDs = append(sessionIDs, sess.SessionID)
	}
	run.Assert(len(sessionIDs) >= maxActive+1, "enough-sessions",
		fmt.Sprintf("need %d got %d", maxActive+1, len(sessionIDs)))

	var wg sync.WaitGroup
	errCh := make(chan error, maxActive)
	for i := 0; i < maxActive && i < len(sessionIDs); i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := c.Sessions.SendPromptAsync(ctx, wsID, sessionIDs[idx],
				"Count from 1 to 100 slowly, writing each number.")
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)

	promptErrors := 0
	for e := range errCh {
		if e != nil {
			promptErrors++
		}
	}
	run.Assert(promptErrors == 0, "p1-fill-slots",
		fmt.Sprintf("%d prompt errors", promptErrors))

	time.Sleep(2 * time.Second)

	if len(sessionIDs) > maxActive {
		extraIdx := maxActive
		status, body, rawErr := canary.RawDo(ctx, "POST",
			fmt.Sprintf("%s/api/v1/workspaces/%s/sessions/%s/prompt",
				cfg.APIURL, wsID, sessionIDs[extraIdx]),
			cfg.APIKey, []byte(`{"message":"hello"}`))
		if rawErr != nil {
			run.Fail("p2-over-limit", rawErr.Error())
		} else {
			run.Assert(status == 429, "p2-429-active-limit",
				fmt.Sprintf("got %d", status))
			if status == 429 {
				var bodyObj map[string]any
				_ = json.Unmarshal(body, &bodyObj)
				_, hasMaxActive := bodyObj["maxActiveSessions"]
				_, hasRetryAfter := bodyObj["retryAfter"]
				run.Assert(hasMaxActive, "p2-has-maxActiveSessions",
					fmt.Sprintf("keys: %v", bodyObj))
				run.Assert(hasRetryAfter, "p2-has-retryAfter",
					fmt.Sprintf("keys: %v", bodyObj))
			}
		}

		for i := 0; i < maxActive && i < len(sessionIDs); i++ {
			_ = c.Sessions.Abort(ctx, wsID, sessionIDs[i])
		}
		time.Sleep(2 * time.Second)

		_, err = c.Sessions.SendMessage(ctx, wsID, sessionIDs[extraIdx], "Reply: PONG")
		run.AssertNoError(err, "p3-after-abort-succeeds")
	}

	const connectionLimit = 11
	var conns []*http.Response
	var connCleanups []func()

	for i := 0; i < connectionLimit; i++ {
		sseURL := fmt.Sprintf("%s/api/v1/workspaces/%s/events", cfg.APIURL, wsID)
		req, _ := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		conns = append(conns, resp)
		connCleanups = append(connCleanups, func() { resp.Body.Close() })
	}

	sseURL := fmt.Sprintf("%s/api/v1/workspaces/%s/events", cfg.APIURL, wsID)
	req, _ := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Accept", "text/event-stream")

	lastResp, lastErr := http.DefaultClient.Do(req)
	if lastErr != nil {
		run.Fail("p5-11th-conn", lastErr.Error())
	} else {
		defer lastResp.Body.Close()
		run.Assert(lastResp.StatusCode == 429, "p5-11th-conn-429",
			fmt.Sprintf("got %d", lastResp.StatusCode))
		if lastResp.StatusCode == 429 {
			body, _ := bufio.NewReader(lastResp.Body).ReadString('\x00')
			var bodyObj map[string]any
			_ = json.Unmarshal([]byte(body), &bodyObj)
			_, hasRetryAfter := bodyObj["retryAfter"]
			run.Assert(hasRetryAfter, "p5-has-retryAfter",
				fmt.Sprintf("keys: %v", bodyObj))
		}
	}

	for _, cleanup := range connCleanups {
		cleanup()
	}
	_ = conns

}
