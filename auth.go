// Request signing for the authenticated endpoints, matching the official
// polymarket-us SDKs: the message timestampMs + METHOD + path — path
// EXCLUDES query parameters — is signed with Ed25519 and sent
// base64-encoded in three X-PM-* headers. The same headers on the HTTP
// upgrade request (method "GET", the stream path) authenticate WebSocket
// connections.
package polymarket

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Signer holds an API key id and its Ed25519 private key, and signs requests
// in place. Safe for concurrent use. Generate API keys at
// https://polymarket.us/developer.
type Signer struct {
	keyID string
	key   ed25519.PrivateKey
	now   func() time.Time // test seam
}

// NewSigner decodes secretKey (base64; either a 32-byte Ed25519 seed or a
// 64-byte private key, of which the seed is the first half — both shapes
// are issued in the wild and both official SDKs accept them) and returns a
// Signer for keyID. The key material never leaves the process.
func NewSigner(keyID, secretKey string) (*Signer, error) {
	if keyID == "" {
		return nil, errors.New("polymarket: empty API key id")
	}
	raw, err := base64.StdEncoding.DecodeString(secretKey)
	if err != nil {
		return nil, fmt.Errorf("polymarket: secret key is not base64: %w", err)
	}
	var seed []byte
	switch len(raw) {
	case ed25519.SeedSize:
		seed = raw
	case ed25519.PrivateKeySize:
		seed = raw[:ed25519.SeedSize]
	default:
		return nil, fmt.Errorf("polymarket: secret key decodes to %d bytes, want 32 or 64", len(raw))
	}
	return &Signer{keyID: keyID, key: ed25519.NewKeyFromSeed(seed), now: time.Now}, nil
}

// sign adds the three X-PM-* headers to req. The signed path is req.URL.Path
// verbatim — Go's URL parsing keeps query params out of Path, which matches
// the SDKs signing the path before query params are attached.
func (s *Signer) sign(req *http.Request) {
	ts, sig := s.signature(req.Method, req.URL.Path)
	req.Header.Set("X-PM-Access-Key", s.keyID)
	req.Header.Set("X-PM-Timestamp", ts)
	req.Header.Set("X-PM-Signature", sig)
}

// signature returns the timestamp and base64 signature for method+path,
// for callers that build headers outside an *http.Request (the ws dialer).
func (s *Signer) signature(method, path string) (ts, sig string) {
	ts = strconv.FormatInt(s.now().UnixMilli(), 10)
	raw := ed25519.Sign(s.key, []byte(ts+method+path))
	return ts, base64.StdEncoding.EncodeToString(raw)
}
