package handlers

import (
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

func b64url(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
func itoaExp(n int64) string { return strconv.FormatInt(n, 10) }

func mintTestJWT(exp time.Time) string {
	// Header/payload shape only — unverifiedJWTExp never checks the signature.
	return "eyJhbGciOiJIUzI1NiJ9." + b64url(`{"exp":`+itoaExp(exp.Unix())+`}`) + ".sig"
}

func TestUnverifiedJWTExp(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	got, ok := unverifiedJWTExp(mintTestJWT(exp))
	if !ok || got.Unix() != exp.Unix() {
		t.Fatalf("exp roundtrip: ok=%v got=%v want=%v", ok, got.Unix(), exp.Unix())
	}
	if _, ok := unverifiedJWTExp("not-a-jwt"); ok {
		t.Fatal("malformed token must not parse")
	}
	if _, ok := unverifiedJWTExp("a.eyJub2V4cCI6MX0.c"); ok {
		t.Fatal("absent exp must return ok=false")
	}
}

func TestExpiryLeewayBoundary(t *testing.T) {
	// 30s past exp: inside the 60s leeway — still accepted.
	if exp, ok := unverifiedJWTExp(mintTestJWT(time.Now().Add(-30 * time.Second))); !ok || time.Now().After(exp.Add(jwtExpiryLeeway)) {
		t.Fatal("within leeway must not be rejected")
	}
	// 2min past exp: outside the leeway — rejected.
	if exp, ok := unverifiedJWTExp(mintTestJWT(time.Now().Add(-2 * time.Minute))); !ok || !time.Now().After(exp.Add(jwtExpiryLeeway)) {
		t.Fatal("past leeway must be rejected")
	}
}
