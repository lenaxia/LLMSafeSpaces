// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import "time"

// User represents a user
type User struct {
	ID            string     `json:"id" db:"id"`
	Username      string     `json:"username" db:"username"`
	Email         string     `json:"email" db:"email"`
	PasswordHash  string     `json:"-" db:"password_hash"`
	CreatedAt     time.Time  `json:"createdAt" db:"created_at"`
	UpdatedAt     time.Time  `json:"updatedAt" db:"updated_at"`
	Active        bool       `json:"active" db:"active"`
	Role          string     `json:"role" db:"role"`
	Status        UserStatus `json:"status" db:"status"`
	EmailVerified bool       `json:"emailVerified" db:"email_verified"`
}

// RegisterRequest is the request body for user registration.
type RegisterRequest struct {
	Username string `json:"username" binding:"required,min=3,max=64"`
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required,min=8,max=128"`
}

// LoginRequest is the request body for user login.
type LoginRequest struct {
	Email      string `json:"email"      binding:"required,email"`
	Password   string `json:"password"   binding:"required"`
	RememberMe bool   `json:"rememberMe"`
}

// AuthResponse is returned after successful registration or login.
//
// RecoveryKey is populated only on registration (one-time display). It is
// the user's sole opportunity to retrieve it; the API does not store it
// anywhere recoverable. Login responses omit this field entirely.
//
// TokenTTL is the effective JWT lifetime used for this session. It is tagged
// json:"-" so it never appears in the HTTP response body — clients already
// receive the exp claim inside the JWT. This field carries the TTL from the
// auth service to the router handler for cookie Max-Age calculation without
// requiring an interface change.
type AuthResponse struct {
	Token       string        `json:"token"`
	User        User          `json:"user"`
	RecoveryKey string        `json:"recoveryKey,omitempty"`
	TokenTTL    time.Duration `json:"-"` // router-internal: not serialized
}

// CreateAPIKeyRequest is the request body for creating an API key.
type CreateAPIKeyRequest struct {
	Name          string   `json:"name" binding:"required,min=1,max=128"`
	DecryptAccess bool     `json:"decryptAccess"`
	AllowedCIDRs  []string `json:"allowedCidrs,omitempty"`
}

// APIKey represents an API key record returned in list responses.
type APIKey struct {
	ID        string     `json:"id"`
	UserID    string     `json:"-" db:"user_id"`
	Name      string     `json:"name"`
	Key       string     `json:"key,omitempty"`
	Prefix    string     `json:"prefix"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Legacy    bool       `json:"legacy,omitempty" db:"key_legacy"`

	DecryptAccess bool     `json:"decryptAccess"`
	DekSynced     bool     `json:"dekSynced"`
	AllowedCIDRs  []string `json:"allowedCidrs,omitempty"`
	KekSalt       []byte   `json:"-" db:"kek_salt"`
	WrappedDEK    []byte   `json:"-" db:"wrapped_dek"`
	KeyCiphertext []byte   `json:"-" db:"key_ciphertext"`
	KeyVersion    int      `json:"-" db:"key_version"`
}

// UserUpdates carries the fields that may be changed on a User record.
// All fields are pointers — nil means "do not update this field".
type UserUpdates struct {
	Username      *string     `json:"username,omitempty"`
	Email         *string     `json:"email,omitempty"`
	Active        *bool       `json:"active,omitempty"`
	Role          *string     `json:"role,omitempty"`
	Status        *UserStatus `json:"status,omitempty"`
	PasswordHash  *string     `json:"-"`
	EmailVerified *bool       `json:"-"`
}

// CachedSession is the typed representation of a WebSocket session stored in
// the cache. It replaces the previous map[string]interface{} bag.
type CachedSession struct {
	SessionID   string `json:"sessionId"`
	UserID      string `json:"userId"`
	WorkspaceID string `json:"workspaceId"`
}

// AuthConfig is returned by GET /auth/config for feature-flag discovery.
type AuthConfig struct {
	RegistrationEnabled bool     `json:"registrationEnabled"`
	OIDCEnabled         bool     `json:"oidcEnabled"`
	SSOProviders        []string `json:"ssoProviders,omitempty"`
	InstanceName        string   `json:"instanceName"`
	MOTD                string   `json:"motd"`
}

// UserStatus is the authoritative operational status of a user account.
type UserStatus string

const (
	UserStatusActive    UserStatus = "active"
	UserStatusSuspended UserStatus = "suspended"
)

// UserListEntry is the list DTO for platform admin user listing.
type UserListEntry struct {
	ID        string     `json:"id"`
	Email     string     `json:"email"`
	Role      string     `json:"role"`
	Status    UserStatus `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	OrgCount  int        `json:"orgCount"`
	OrgID     string     `json:"orgId,omitempty"`
	OrgName   string     `json:"orgName,omitempty"`
}
