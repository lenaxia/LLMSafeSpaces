// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package canary

import (
	"net/http"

	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// NewAbiClient builds the production Client: the reference abiclient
// over a §D1 Basic-auth transport (the newUsageStreamClient twin — that
// one lives unexported in handlers; this one serves the canary's unary
// ops). Per-attempt bounds come from the probe's ctx, not the http
// client.
func NewAbiClient(baseURL, password string) Client {
	httpClient := &http.Client{
		Transport: &basicAuthTransport{password: password, inner: http.DefaultTransport},
		Timeout:   0,
	}
	return abiclient.New(httpClient, baseURL)
}

type basicAuthTransport struct {
	password string
	inner    http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(agentd.AuthUsername, t.password)
	return t.inner.RoundTrip(r)
}
