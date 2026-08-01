// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpserver

import (
	"bufio"
	"context"
	"net"
)

// netListen returns a listener on a random port (port 0). Uses a
// net.ListenConfig with a context to satisfy the noctx linter.
func netListen() (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
}

// lineScanner wraps bufio.Scanner for reading newline-delimited JSON-RPC
// from stdin in the stdio transport.
type lineScanner interface {
	Scan() bool
	Bytes() []byte
}

func newLineScanner(r interface {
	Read(p []byte) (n int, err error)
}) lineScanner {
	return bufio.NewScanner(r)
}
