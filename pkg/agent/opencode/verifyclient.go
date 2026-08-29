package opencode

import (
	"net"
	"net/http"
	"time"
)

// verifyRequestTimeout bounds ONE verify request end-to-end. The verify
// oracle is a small local GET against the workspace agent; unlike Send
// (blocks on LLM completion) it must never consume an unbounded caller
// budget. Default 15s covers a cold agent's worst observed first-response
// latency (971ms, 2026-08-29 first-traffic) with two orders of margin.
// Var for tests.
var verifyRequestTimeout = 15 * time.Second

// verifyDialTimeout bounds connection establishment for verify requests.
// Local pod-network: anything slower is a wedge, not latency.
const verifyDialTimeout = 3 * time.Second

// newVerifyHTTPClient builds the dedicated client for delivery
// verification (#1119 follow-up 2, 2026-08-29 first-traffic incident).
//
// The shared adapter client pools keep-alive connections with NO
// client-level timeout (correct for Send/GetHistory, whose budgets belong
// to the caller). First live V2 traffic showed the failure mode that
// combination hides: ONE wedged pooled connection to a workspace agent
// (observed after an opencode instance dispose mid-window — POSTs on new
// connections and the SSE stream kept flowing) hung every verify GET
// until TCP death, burning the entire promotion-await budget and then
// seven consecutive inconclusive verify passes against a healthy agent.
//
// The oracle therefore gets its own client with fresh connections per
// request (DisableKeepAlives — a wedged connection can cost at most one
// bounded pass, never the pool) and hard per-request bounds.
func newVerifyHTTPClient() *http.Client {
	return &http.Client{
		Timeout: verifyRequestTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: (&net.Dialer{
				Timeout: verifyDialTimeout,
			}).DialContext,
			MaxConnsPerHost: 2, // verify is serial per session; a wedge must not fan out
		},
	}
}

// withVerifyTransport returns a shallow copy of the client whose HTTP
// layer is the dedicated verify client. Copy-not-mutate: the underlying
// Client may be shared (resolveWorkspaceClient reuses the adapter's
// pooled transport for everything else); swapping the pool on the shared
// value would un-pool Send/admissions as a side effect.
func (c *Client) withVerifyTransport(hc *http.Client) *Client {
	// Explicit construction, not a shallow copy: Client embeds a
	// sync.Once (capability probe) that must not be lock-copied
	// (govet copylocks). The fresh verify client gets zero capability
	// state — verify never sends model refs, so it never probes.
	return &Client{
		baseURL:    c.baseURL,
		password:   c.password,
		httpClient: hc,
		logger:     c.logger,
	}
}

// withVerifyClient returns the resolved client bound to the dedicated
// verify transport (lazily built once per adapter).
func (a *Adapter) withVerifyClient(c *Client) *Client {
	a.verifyOnce.Do(func() { a.verifyHTTPCli = newVerifyHTTPClient() })
	return c.withVerifyTransport(a.verifyHTTPCli)
}
