// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package email provides the orchestration layer for outbound transactional
// email. It wraps a pkg/email.EmailProvider (SES or Noop) and centralizes
// message construction so that email bodies and link shapes are defined in
// one place rather than scattered as inline fmt.Sprintf calls across handlers.
//
// The Service holds a resolved provider + baseURL. Handlers depend on the
// concrete *Service (the provider is the injectable seam, already an
// interface). baseURL is retained for the password-reset / email-verification
// flows (US-49.5/49.6) which build interstitial frontend links.
package email

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/email"
)

// ErrNotConfigured is returned when the Service has no provider (email is
// disabled). Callers use ProviderName() to distinguish noop from ses for UX;
// this sentinel guards against calling Send* on a nil-provider Service.
var ErrNotConfigured = errors.New("email is not configured")

const (
	passwordResetExpiryText = "15 minutes"
)

// Service builds and sends transactional emails via the wrapped provider.
type Service struct {
	provider     email.EmailProvider
	baseURL      string
	providerName string // resolved label: "ses" | "noop"
}

// NewService constructs a Service. provider may be nil (email disabled); in
// that case Send* returns ErrNotConfigured. baseURL is the public origin used
// to build links in email bodies (consumed by US-49.5/49.6). providerName is
// the resolved provider label for UX ("ses", "noop"); empty or unknown values
// normalise to "noop".
func NewService(provider email.EmailProvider, baseURL, providerName string) *Service {
	return &Service{
		provider:     provider,
		baseURL:      baseURL,
		providerName: normaliseProviderName(providerName),
	}
}

// ProviderName returns the resolved provider label for UX ("ses" or "noop").
func (s *Service) ProviderName() string {
	return s.providerName
}

// SendTest sends a test email so an admin can verify SES wiring end-to-end.
func (s *Service) SendTest(ctx context.Context, to string) error {
	if s.provider == nil {
		return ErrNotConfigured
	}
	return s.provider.Send(ctx, email.Message{
		To:       to,
		Subject:  "LLMSafeSpaces test email",
		TextBody: "This is a test email from LLMSafeSpaces. If you received this, outbound email is working.",
		HTMLBody: "<p>This is a test email from LLMSafeSpaces. If you received this, outbound email is working.</p>",
	})
}

// SendPasswordReset sends a password-reset link. The link targets the
// interstitial frontend page /reset-password, which POSTs to the confirm
// endpoint — never a consuming GET (scanner-defense invariant, US-49.9).
func (s *Service) SendPasswordReset(ctx context.Context, to, token string) error {
	if s.provider == nil {
		return ErrNotConfigured
	}
	link := s.buildLink("/reset-password", token)
	textBody := fmt.Sprintf(
		"A request was made to reset your LLMSafeSpaces password.\n\nClick here to set a new password: %s\n\nThis link expires in %s. If you did not request a password reset, you can safely ignore this email.",
		link, passwordResetExpiryText,
	)
	htmlBody := fmt.Sprintf(
		"<p>A request was made to reset your LLMSafeSpaces password.</p><p><a href=\"%s\">Click here to set a new password</a></p><p>This link expires in %s. If you did not request a password reset, you can safely ignore this email.</p>",
		html.EscapeString(link), passwordResetExpiryText,
	)
	return s.provider.Send(ctx, email.Message{
		To:       to,
		Subject:  "Reset your LLMSafeSpaces password",
		TextBody: textBody,
		HTMLBody: htmlBody,
	})
}

// SendPasswordChanged sends a post-reset notification so the legitimate owner
// can detect an account takeover. Per OWASP, this is a notification, not a
// gate — it does not block the reset.
func (s *Service) SendPasswordChanged(ctx context.Context, to string) error {
	if s.provider == nil {
		return ErrNotConfigured
	}
	return s.provider.Send(ctx, email.Message{
		To:       to,
		Subject:  "Your LLMSafeSpaces password was changed",
		TextBody: "Your LLMSafeSpaces password was changed. If this was you, no action is needed. If this was not you, contact your administrator immediately or reset your password via email.",
		HTMLBody: "<p>Your LLMSafeSpaces password was changed.</p><p>If this was you, no action is needed.</p><p><strong>If this was not you, contact your administrator immediately or reset your password via email.</strong></p>",
	})
}

// SendEmailVerification sends an email-verification link. The link targets
// the interstitial frontend page /verify-email, which POSTs to the verify
// endpoint — never a consuming GET (scanner-defense invariant, US-49.9).
func (s *Service) SendEmailVerification(ctx context.Context, to, token string) error {
	if s.provider == nil {
		return ErrNotConfigured
	}
	link := s.buildLink("/verify-email", token)
	textBody := fmt.Sprintf(
		"Please verify your email address for LLMSafeSpaces.\n\nClick here to verify: %s\n\nThis link expires in 24 hours.",
		link,
	)
	htmlBody := fmt.Sprintf(
		"<p>Please verify your email address for LLMSafeSpaces.</p><p><a href=\"%s\">Click here to verify</a></p><p>This link expires in 24 hours.</p>",
		html.EscapeString(link),
	)
	return s.provider.Send(ctx, email.Message{
		To:       to,
		Subject:  "Verify your LLMSafeSpaces email",
		TextBody: textBody,
		HTMLBody: htmlBody,
	})
}

// buildLink constructs a frontend URL carrying the token in the query string.
// Trims trailing slashes from baseURL; URL-encodes the token as
// defense-in-depth (tokens are crypto/rand base64url which is URL-safe, but
// escaping guards against future callers passing differently-shaped tokens).
func (s *Service) buildLink(path, token string) string {
	base := strings.TrimRight(s.baseURL, "/")
	return fmt.Sprintf("%s%s?token=%s", base, path, url.QueryEscape(token))
}

// normaliseProviderName maps the raw config value to the resolved label.
// Empty or unrecognized values become "noop" so the UX never shows a raw
// provider string the user can't interpret.
func normaliseProviderName(name string) string {
	switch strings.ToLower(name) {
	case "ses":
		return "ses"
	default:
		return "noop"
	}
}
