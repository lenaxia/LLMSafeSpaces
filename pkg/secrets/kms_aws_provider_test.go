// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeKMSServer is a test double for the AWS KMS HTTP API. It implements
// just enough of the Encrypt and Decrypt endpoints to exercise the
// AWSKMSProvider against a real SDK client without making network calls.
//
// The "encryption" is XOR with a fixed 32-byte key derived from the key
// ID — NOT cryptographically secure, but sufficient to verify the provider
// round-trips through the API correctly. The real cryptographic work is
// done by AWS in production; we only need to verify our provider code
// handles the request/response shape and the prefix wrapping.
type fakeKMSServer struct {
	t        *testing.T
	keyBytes map[string][]byte // keyID → 32-byte XOR key
	mux      *http.ServeMux
}

func newFakeKMSServer(t *testing.T) *fakeKMSServer {
	t.Helper()
	srv := &fakeKMSServer{
		t:        t,
		keyBytes: make(map[string][]byte),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handleKMS)
	srv.mux = mux
	return srv
}

func (f *fakeKMSServer) start() *httptest.Server {
	return httptest.NewServer(f.mux)
}

func (f *fakeKMSServer) registerKey(keyID string) {
	key := make([]byte, 32)
	// Derive a stable-but-distinct key per keyID so different keys produce
	// different ciphertexts.
	for i := range key {
		key[i] = byte(i) ^ keyID[0]
	}
	f.keyBytes[keyID] = key
}

// handleKMS routes the AWS SDK's "X-Amz-Target" header to Encrypt or Decrypt.
func (f *fakeKMSServer) handleKMS(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")
	body, _ := io.ReadAll(r.Body)

	switch {
	case strings.HasSuffix(target, "Encrypt"):
		f.handleEncrypt(w, body)
	case strings.HasSuffix(target, "Decrypt"):
		f.handleDecrypt(w, body)
	default:
		f.t.Errorf("fakeKMSServer: unexpected X-Amz-Target %q", target)
		w.WriteHeader(http.StatusBadRequest)
	}
}

type kmsEncryptRequest struct {
	KeyID     string `json:"KeyId"`
	Plaintext []byte `json:"Plaintext"`
}

type kmsEncryptResponse struct {
	CiphertextBlob []byte `json:"CiphertextBlob"`
	KeyID          string `json:"KeyId"`
}

type kmsDecryptRequest struct {
	CiphertextBlob []byte `json:"CiphertextBlob"`
}

type kmsDecryptResponse struct {
	Plaintext []byte `json:"Plaintext"`
	KeyID     string `json:"KeyId"`
}

func (f *fakeKMSServer) handleEncrypt(w http.ResponseWriter, body []byte) {
	var req kmsEncryptRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key, ok := f.keyBytes[req.KeyID]
	if !ok {
		f.t.Errorf("fakeKMSServer: unknown key ID %q", req.KeyID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// XOR "encrypt": keyID || XOR(plaintext, key). The keyID prefix in the
	// ciphertext mirrors how real KMS tags ciphertexts with the key that
	// produced them.
	ct := make([]byte, 0, len(req.KeyID)+1+len(req.Plaintext))
	ct = append(ct, []byte(req.KeyID)...)
	ct = append(ct, 0x00) // separator
	for i, b := range req.Plaintext {
		ct = append(ct, b^key[i%len(key)])
	}
	resp := kmsEncryptResponse{CiphertextBlob: ct, KeyID: req.KeyID}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeKMSServer) handleDecrypt(w http.ResponseWriter, body []byte) {
	var req kmsDecryptRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// Extract keyID prefix.
	sep := bytes.IndexByte(req.CiphertextBlob, 0x00)
	if sep < 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	keyID := string(req.CiphertextBlob[:sep])
	ctBody := req.CiphertextBlob[sep+1:]
	key, ok := f.keyBytes[keyID]
	if !ok {
		f.t.Errorf("fakeKMSServer: decrypt with unknown key ID %q", keyID)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	pt := make([]byte, len(ctBody))
	for i, b := range ctBody {
		pt[i] = b ^ key[i%len(key)]
	}
	resp := kmsDecryptResponse{Plaintext: pt, KeyID: keyID}
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_ = json.NewEncoder(w).Encode(resp)
}

// newTestAWSKMSProvider constructs an AWSKMSProvider pointing at the fake
// server. Reused across all test cases.
func newTestAWSKMSProvider(t *testing.T, srv *httptest.Server, keyID string) *AWSKMSProvider {
	t.Helper()
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test-key", "test-secret", ""),
		HTTPClient:  srv.Client(),
	}
	// Override the endpoint to point at the test server. The KMS SDK
	// client takes an explicit BaseEndpoint.
	client := kms.NewFromConfig(cfg, func(o *kms.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
	})
	return &AWSKMSProvider{
		client: client,
		keyID:  keyID,
	}
}

// --- AWSKMSProvider tests ---

func TestAWSKMSProvider_RoundTrip_EncryptThenDecrypt(t *testing.T) {
	fake := newFakeKMSServer(t)
	fake.registerKey("arn:aws:kms:us-east-1:123:key/test-key-id")
	srv := fake.start()
	defer srv.Close()

	p := newTestAWSKMSProvider(t, srv, "arn:aws:kms:us-east-1:123:key/test-key-id")
	ctx := context.Background()

	plaintext := []byte("the-llm-api-key-value")
	ct, err := p.Encrypt(ctx, plaintext)
	require.NoError(t, err)

	dec, err := p.Decrypt(ctx, ct)
	require.NoError(t, err)
	assert.Equal(t, plaintext, dec)
}

func TestAWSKMSProvider_Encrypt_WrapsWithAwsKmsV1Prefix(t *testing.T) {
	fake := newFakeKMSServer(t)
	fake.registerKey("test-key")
	srv := fake.start()
	defer srv.Close()

	p := newTestAWSKMSProvider(t, srv, "test-key")
	ct, err := p.Encrypt(context.Background(), []byte("payload"))
	require.NoError(t, err)

	// The provider's output MUST start with "aws-kms:v1:" + base64(KMS ct).
	// This prefix is what CompositeProvider routes on.
	require.True(t, bytes.HasPrefix(ct, []byte(awsKMSCiphertextPrefix)),
		"AWSKMSProvider.Encrypt output must start with %q; got %q",
		awsKMSCiphertextPrefix, ct[:min(len(ct), 32)])

	// Strip prefix and verify the body is valid base64.
	body := ct[len(awsKMSCiphertextPrefix):]
	_, err = base64.StdEncoding.DecodeString(string(body))
	require.NoError(t, err, "body after prefix must be valid base64")
}

func TestAWSKMSProvider_PrefixMismatch_ReturnsErrNotMyCiphertext(t *testing.T) {
	fake := newFakeKMSServer(t)
	fake.registerKey("test-key")
	srv := fake.start()
	defer srv.Close()

	p := newTestAWSKMSProvider(t, srv, "test-key")

	// A ciphertext with the LOCAL provider's prefix — not ours.
	foreignCT := []byte(staticCiphertextPrefix + base64.StdEncoding.EncodeToString([]byte("not-ours")))
	_, err := p.Decrypt(context.Background(), foreignCT)
	require.ErrorIs(t, err, ErrNotMyCiphertext,
		"local-prefixed ciphertext must return ErrNotMyCiphertext for KMS provider routing")
}

func TestAWSKMSProvider_Decrypt_KMSUnavailable_ReturnsError(t *testing.T) {
	// Start a server, capture its URL, then close it immediately to simulate
	// KMS being unavailable.
	fake := newFakeKMSServer(t)
	fake.registerKey("test-key")
	srv := fake.start()
	srvURL := srv.URL
	srv.Close()

	p := newTestAWSKMSProvider(t, srv, "test-key")
	// Re-point at the now-dead URL — use a fresh client because srv.Close
	// invalidated the connection pool.
	_ = srvURL

	// Construct a valid-looking aws-kms:v1: ciphertext so we get past the
	// prefix check and actually hit the KMS client.
	ct := []byte(awsKMSCiphertextPrefix + base64.StdEncoding.EncodeToString([]byte("dead-server")))
	_, err := p.Decrypt(context.Background(), ct)
	require.Error(t, err, "KMS unavailable must surface as an error")
	// Must NOT be ErrNotMyCiphertext — the prefix matched, this is a real failure.
	assert.NotErrorIs(t, err, ErrNotMyCiphertext,
		"KMS-unavailable must not be classified as a routing signal")
}

func TestAWSKMSProvider_Decrypt_Throttled429_ReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Return a throttling error in AWS JSON shape.
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"ThrottlingException","message":"Rate exceeded"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := newTestAWSKMSProvider(t, srv, "test-key")
	ct := []byte(awsKMSCiphertextPrefix + base64.StdEncoding.EncodeToString([]byte("anything")))
	_, err := p.Decrypt(context.Background(), ct)
	require.Error(t, err, "throttling must surface as an error")
	assert.NotErrorIs(t, err, ErrNotMyCiphertext)
}

func TestAWSKMSProvider_AuthFromCredentialsFile(t *testing.T) {
	// Write a fake credentials file in the AWS shared-credentials format and
	// verify the provider loads it. The actual file format is INI:
	//   [default]
	//   aws_access_key_id = ...
	//   aws_secret_access_key = ...
	tmpDir := t.TempDir()
	credsFile := tmpDir + "/credentials"
	credsContent := "[default]\naws_access_key_id = AKIATESTFILE\naws_secret_access_key = secretFromfile\n"
	require.NoError(t, writeTestFile(credsFile, credsContent))

	fake := newFakeKMSServer(t)
	fake.registerKey("file-auth-key")
	srv := fake.start()
	defer srv.Close()

	// Construct the provider from the credentials file.
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithSharedCredentialsFiles([]string{credsFile}),
	)
	require.NoError(t, err)
	client := kms.NewFromConfig(cfg, func(o *kms.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.HTTPClient = srv.Client()
	})
	p := &AWSKMSProvider{client: client, keyID: "file-auth-key"}

	ct, err := p.Encrypt(context.Background(), []byte("roundtrip-via-file-auth"))
	require.NoError(t, err)
	dec, err := p.Decrypt(context.Background(), ct)
	require.NoError(t, err)
	assert.Equal(t, []byte("roundtrip-via-file-auth"), dec)
}

// writeTestFile is a tiny helper for tests that need a credentials file on
// disk. Uses os.WriteFile directly.
func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}

// Ensure imports used only by helpers are exercised.
var _ = fmt.Sprintf
