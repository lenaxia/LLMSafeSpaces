// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodyCaptureWriter is a gin.ResponseWriter that captures the response body
// while allowing it to be written to the client
type BodyCaptureWriter struct {
	gin.ResponseWriter
	Body *bytes.Buffer
}

// Write captures the response body and forwards it to the underlying ResponseWriter
func (w *BodyCaptureWriter) Write(b []byte) (int, error) {
	w.Body.Write(b)
	return w.ResponseWriter.Write(b)
}

// WriteString captures the string response and forwards it to the underlying ResponseWriter
func (w *BodyCaptureWriter) WriteString(s string) (int, error) {
	w.Body.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// GetBody returns the captured response body as a string
func (w *BodyCaptureWriter) GetBody() string {
	return w.Body.String()
}

// Unwrap exposes the underlying gin.ResponseWriter so http.ResponseController
// can reach connection-level controls (SetReadDeadline etc.) through the
// capture layers. Without it, the double capture the logging + error-handler
// middlewares install makes every ResponseController feature return
// ErrNotSupported — silently disarming the upload drain's time bound (the
// #1608 review's correctness finding 1).
func (w *BodyCaptureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// NewBodyCaptureWriter creates a new BodyCaptureWriter
func NewBodyCaptureWriter(c *gin.Context) *BodyCaptureWriter {
	return &BodyCaptureWriter{
		ResponseWriter: c.Writer,
		Body:           bytes.NewBufferString(""),
	}
}
