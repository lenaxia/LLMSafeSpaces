// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// testAuthenticator generates valid WebAuthn attestation/assertion responses
// for ceremony happy-path tests using P-256 + "none" attestation format.
type testAuthenticator struct {
	rpID         string
	origin       string
	credentialID []byte
	privateKey   *ecdsa.PrivateKey
	signCount    uint32
}

func newTestAuthenticator() *testAuthenticator {
	credID := make([]byte, 32)
	_, _ = rand.Read(credID)
	return &testAuthenticator{
		rpID:         "localhost",
		origin:       "https://localhost",
		credentialID: credID,
	}
}

// generateRegistrationResponse builds a valid WebAuthn attestation response
// map for the "none" format matching the given challenge.
func (a *testAuthenticator) generateRegistrationResponse(challengeB64 string) (map[string]any, error) {
	challenge, err := base64.RawURLEncoding.DecodeString(challengeB64)
	if err != nil {
		return nil, fmt.Errorf("decode challenge: %w", err)
	}

	clientDataJSON, _ := a.buildClientDataJSON("webauthn.create", challenge)

	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	a.privateKey = privKey

	xPadded := padTo32(privKey.X.Bytes())
	yPadded := padTo32(privKey.Y.Bytes())

	rpIDHash := sha256.Sum256([]byte(a.rpID))
	flags := byte(0x41) // UP=1, AT=1

	aaguid := make([]byte, 16)
	credIDLen := len(a.credentialID)

	coseKey := map[int64]any{
		1:  int64(2),
		3:  int64(-7),
		-1: int64(1),
		-2: xPadded,
		-3: yPadded,
	}
	coseKeyCBOR, err := cbor.Marshal(coseKey)
	if err != nil {
		return nil, err
	}

	authData := make([]byte, 0, 37+16+2+credIDLen+len(coseKeyCBOR))
	authData = append(authData, rpIDHash[:]...)
	authData = append(authData, flags)
	authData = append(authData, 0, 0, 0, 0)
	authData = append(authData, aaguid...)
	authData = append(authData, byte(credIDLen>>8), byte(credIDLen)) //nolint:gosec // G115: credIDLen is 32
	authData = append(authData, a.credentialID...)
	authData = append(authData, coseKeyCBOR...)

	attObj := map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	}
	attObjCBOR, err := cbor.Marshal(attObj)
	if err != nil {
		return nil, err
	}

	credIDB64 := base64.RawURLEncoding.EncodeToString(a.credentialID)
	return map[string]any{
		"id":    credIDB64,
		"rawId": credIDB64,
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObjCBOR),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientDataJSON),
		},
	}, nil
}

// generateAssertionResponse builds a valid WebAuthn assertion response map
// matching the given challenge, signed with the credential's private key.
func (a *testAuthenticator) generateAssertionResponse(challengeB64 string) (map[string]any, error) {
	if a.privateKey == nil {
		return nil, fmt.Errorf("no registered credential")
	}
	challenge, err := base64.RawURLEncoding.DecodeString(challengeB64)
	if err != nil {
		return nil, err
	}

	clientDataJSON, _ := a.buildClientDataJSON("webauthn.get", challenge)

	rpIDHash := sha256.Sum256([]byte(a.rpID))
	flags := byte(0x01) // UP=1

	a.signCount++
	authData := make([]byte, 0, 37)
	authData = append(authData, rpIDHash[:]...)
	authData = append(authData, flags)
	authData = append(authData, byte(a.signCount>>24), byte(a.signCount>>16), byte(a.signCount>>8), byte(a.signCount)) //nolint:gosec // G115: uint32 fits

	clientDataHash := sha256.Sum256(clientDataJSON)
	signedData := append(authData, clientDataHash[:]...)
	digest := sha256.Sum256(signedData)
	sig, err := ecdsa.SignASN1(rand.Reader, a.privateKey, digest[:])
	if err != nil {
		return nil, err
	}

	credIDB64 := base64.RawURLEncoding.EncodeToString(a.credentialID)
	return map[string]any{
		"id":    credIDB64,
		"rawId": credIDB64,
		"type":  "public-key",
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(authData),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientDataJSON),
			"signature":         base64.RawURLEncoding.EncodeToString(sig),
		},
	}, nil
}

func (a *testAuthenticator) buildClientDataJSON(typ string, challenge []byte) ([]byte, error) {
	return json.Marshal(map[string]any{
		"type":        typ,
		"challenge":   base64.RawURLEncoding.EncodeToString(challenge),
		"origin":      a.origin,
		"crossOrigin": false,
	})
}

func padTo32(b []byte) []byte {
	if len(b) >= 32 {
		return b[:32]
	}
	padded := make([]byte, 32)
	copy(padded[32-len(b):], b)
	return padded
}
