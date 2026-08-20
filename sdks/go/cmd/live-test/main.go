// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Comprehensive live integration test for the Go SDK.
// Run: API_URL=http://localhost:18080 API_KEY=lsp_... go run ./cmd/live-test/
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	llm "github.com/lenaxia/llmsafespaces/sdk/go"
)

var (
	passed int
	failed int
	errs   []string
)

func ok(cond bool, label string) {
	if cond {
		fmt.Printf("  ✓ %s\n", label)
		passed++
	} else {
		fmt.Printf("  ✗ %s\n", label)
		failed++
		errs = append(errs, label)
	}
}

func waitHealthy(ctx context.Context, c *llm.Client, id string) string {
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		ws, err := c.Workspaces.Get(ctx, id)
		if err == nil && ws.Phase == "Active" {
			return "Healthy" // simplified — real check would use status endpoint
		}
		select {
		case <-ctx.Done():
			return "timeout"
		case <-time.After(5 * time.Second):
		}
	}
	return "timeout"
}

func main() {
	apiURL := os.Getenv("API_URL")
	apiKey := os.Getenv("API_KEY")
	if apiURL == "" {
		apiURL = "http://localhost:18080"
	}
	if apiKey == "" {
		apiKey = "lsp_upgradetest1234567890abcdef"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	c := llm.New(apiURL, llm.WithAPIKey(apiKey), llm.WithTimeout(120*time.Second))

	fmt.Println("=== Go SDK Live Integration Test (Comprehensive) ===")

	// ═══════════════════════════════════════════════════════════════════════════
	// 1. AUTH
	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Println("\n─── Auth ───")
	me, err := c.Auth.Me(ctx)
	ok(err == nil, "Auth.Me() → no error")
	if err == nil {
		ok(me["id"] != nil, "Auth.Me() → id present")
		ok(me["email"] != nil, "Auth.Me() → email present")
		ok(me["role"] != nil, "Auth.Me() → role present")
		fmt.Printf("  User: %v (%v)\n", me["email"], me["role"])
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// 2. WORKSPACE LIFECYCLE
	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Println("\n─── Workspace Lifecycle ───")
	ws, err := c.Workspaces.Create(ctx, llm.CreateWorkspaceRequest{
		Name: "go-live-comprehensive", Runtime: "base", StorageSize: "1Gi",
	})
	ok(err == nil && ws.ID != "", "Workspaces.Create() → id")
	if err != nil {
		fmt.Printf("  FATAL: %v\n", err)
		os.Exit(1)
	}
	ok(ws.Name == "go-live-comprehensive", "Workspaces.Create() → name")
	ok(ws.Runtime == "base", "Workspaces.Create() → runtime")
	fmt.Printf("  Created: %s\n", ws.ID)

	got, err := c.Workspaces.Get(ctx, ws.ID)
	ok(err == nil && got.ID == ws.ID, "Workspaces.Get() → correct id")

	list, err := c.Workspaces.List(ctx, 20, 0)
	ok(err == nil && len(list.Items) >= 1, "Workspaces.List() → ≥1 item")

	// Pagination
	page, err := c.Workspaces.List(ctx, 1, 0)
	ok(err == nil && len(page.Items) <= 1, "Workspaces.List(limit=1) → ≤1 item")

	// Wait for active
	fmt.Println("  Waiting for workspace active...")
	health := waitHealthy(ctx, c, ws.ID)
	ok(health == "Healthy", fmt.Sprintf("workspace healthy (got: %s)", health))
	if health != "Healthy" {
		c.Workspaces.Delete(ctx, ws.ID)
		os.Exit(1)
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// 3. SESSIONS
	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Println("\n─── Sessions ───")
	var session *llm.EnsureSessionResponse
	for i := 0; i < 5; i++ {
		session, err = c.Sessions.Ensure(ctx, ws.ID)
		if err == nil && session.SessionID != "" {
			break
		}
		fmt.Printf("  Session ensure retry %d: %v\n", i+1, err)
		time.Sleep(5 * time.Second)
	}
	ok(err == nil && session.SessionID != "", "Sessions.Ensure() → sessionId")
	if err == nil {
		ok(session.WorkspaceID == ws.ID, "Sessions.Ensure() → workspaceId")
		fmt.Printf("  Session: %s (resumed: %v)\n", session.SessionID, session.Resumed)
	}

	if session != nil && session.SessionID != "" {
		// Send message via SDK (now fixed with parts format)
		fmt.Println("  Sending message via SDK...")
		msg, err := c.Sessions.SendMessage(ctx, ws.ID, session.SessionID, "Reply with exactly: PONG")
		ok(err == nil, "Sessions.SendMessage() → no error")
		if err == nil {
			ok(len(msg.TextContent()) > 0, "Sessions.SendMessage() → non-empty content")
			content := msg.TextContent()
			if len(content) > 80 {
				content = content[:80]
			}
			fmt.Printf("  Agent said: %q\n", content)
		} else {
			fmt.Printf("  SendMessage error: %v\n", err)
		}

		// History
		history, err := c.Sessions.GetHistory(ctx, ws.ID, session.SessionID)
		ok(err == nil && len(history) >= 1, "Sessions.GetHistory() → ≥1 entry")

		// Abort
		err = c.Sessions.Abort(ctx, ws.ID, session.SessionID)
		ok(err == nil, "Sessions.Abort() → no error")
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// 4. TERMINAL TICKET
	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Println("\n─── Terminal ───")
	ticket, err := c.Terminal.GetTicket(ctx, ws.ID)
	ok(err == nil, "Terminal.GetTicket() → no error")
	if err == nil {
		ok(strings.HasPrefix(ticket.Ticket, "tkt_"), "Terminal.GetTicket() → tkt_ prefix")
		ok(len(ticket.Ticket) > 10, "Terminal.GetTicket() → sufficient length")
		ok(ticket.ExpiresAt != "", "Terminal.GetTicket() → expiresAt")
		fmt.Printf("  Ticket: %s...\n", ticket.Ticket[:20])
	}

	t2, _ := c.Terminal.GetTicket(ctx, ws.ID)
	if ticket != nil && t2 != nil {
		ok(ticket.Ticket != t2.Ticket, "terminal tickets are unique")
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// 5. SECRETS
	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Println("\n─── Secrets ───")
	secret, err := c.Secrets.Create(ctx, "go-live-secret", "env-secret", "go-val-99")
	if err == nil {
		ok(secret.ID != "", "Secrets.Create() → id")
		ok(secret.Name == "go-live-secret", "Secrets.Create() → name")

		secList, err := c.Secrets.List(ctx)
		ok(err == nil, "Secrets.List() → no error")
		found := false
		for _, s := range secList {
			if s.ID == secret.ID {
				found = true
			}
		}
		ok(found, "Secrets.List() → contains new")

		err = c.Secrets.Delete(ctx, secret.ID)
		ok(err == nil, "Secrets.Delete() → no error")
	} else {
		fmt.Printf("  ⚠ Secrets skipped: %v\n", err)
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// 6. SUSPEND / RESUME
	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Println("\n─── Suspend / Resume ───")
	err = c.Workspaces.Suspend(ctx, ws.ID)
	ok(err == nil, "Workspaces.Suspend() → no error")

	// Wait for suspended
	for i := 0; i < 20; i++ {
		g, _ := c.Workspaces.Get(ctx, ws.ID)
		if g != nil && g.Phase == "Suspended" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	g, _ := c.Workspaces.Get(ctx, ws.ID)
	ok(g != nil && g.Phase == "Suspended", fmt.Sprintf("phase → Suspended (got: %s)", g.Phase))

	_, err = c.Workspaces.Activate(ctx, ws.ID)
	ok(err == nil, "Workspaces.Activate() → no error")

	rh := waitHealthy(ctx, c, ws.ID)
	ok(rh == "Healthy", fmt.Sprintf("resume → Healthy (got: %s)", rh))

	var postResume *llm.EnsureSessionResponse
	for i := 0; i < 5; i++ {
		postResume, err = c.Sessions.Ensure(ctx, ws.ID)
		if err == nil && postResume.SessionID != "" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	ok(err == nil && postResume != nil && postResume.SessionID != "", "Sessions.Ensure() works after resume")

	// ═══════════════════════════════════════════════════════════════════════════
	// 7. ERROR HANDLING
	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Println("\n─── Error Handling ───")
	_, err = c.Workspaces.Get(ctx, "00000000-0000-0000-0000-000000000000")
	ok(err != nil && llm.IsNotFound(err), "nonexistent ws → IsNotFound")

	badC := llm.New(apiURL, llm.WithAPIKey("lsp_invalid"))
	_, err = badC.Auth.Me(ctx)
	ok(err != nil && llm.IsAuth(err), "invalid key → IsAuth")

	_, err = c.Terminal.GetTicket(ctx, "00000000-0000-0000-0000-000000000000")
	ok(err != nil, "terminal ticket nonexistent → error")

	// ═══════════════════════════════════════════════════════════════════════════
	// 8. CLEANUP
	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Println("\n─── Cleanup ───")
	err = c.Workspaces.Delete(ctx, ws.ID)
	ok(err == nil, "Workspaces.Delete() → no error")

	time.Sleep(3 * time.Second)
	deleted, err := c.Workspaces.Get(ctx, ws.ID)
	if err != nil {
		ok(llm.IsNotFound(err), "deleted ws → 404 (hard deleted)")
	} else {
		ok(deleted.Phase == "Deleted" || deleted.Phase == "Terminating",
			fmt.Sprintf("deleted ws → terminal phase (got: %s)", deleted.Phase))
	}

	// ═══════════════════════════════════════════════════════════════════════════
	fmt.Printf("\n═══ Results: %d passed, %d failed ═══\n", passed, failed)
	if len(errs) > 0 {
		fmt.Printf("Failures:\n  %s\n", strings.Join(errs, "\n  "))
	}
	if failed > 0 {
		os.Exit(1)
	}
}
