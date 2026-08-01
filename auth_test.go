package polymarket

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"testing"
	"time"
)

// testSeed is a fixed 32-byte Ed25519 seed; tests derive the public key from
// it to verify signatures end to end.
var testSeed = make([]byte, ed25519.SeedSize)

func init() {
	for i := range testSeed {
		testSeed[i] = byte(i + 1)
	}
}

func testSigner(t *testing.T, secretB64 string) *Signer {
	t.Helper()
	s, err := NewSigner("key-id-1", secretB64)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }
	return s
}

func TestSignerSignsSpecMessage(t *testing.T) {
	s := testSigner(t, base64.StdEncoding.EncodeToString(testSeed))

	// Query params must NOT be part of the signed message (the SDKs sign the
	// path before query params are attached).
	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/v1/orders/open?slugs=abc", nil)
	s.sign(req)

	if got := req.Header.Get("X-PM-Access-Key"); got != "key-id-1" {
		t.Errorf("X-PM-Access-Key = %q", got)
	}
	ts := req.Header.Get("X-PM-Timestamp")
	if want := "1785585600000"; ts != want {
		t.Errorf("X-PM-Timestamp = %q, want %q (ms)", ts, want)
	}
	sig, err := base64.StdEncoding.DecodeString(req.Header.Get("X-PM-Signature"))
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(testSeed).Public().(ed25519.PublicKey)
	msg := []byte(ts + "GET" + "/v1/orders/open")
	if !ed25519.Verify(pub, msg, sig) {
		t.Error("signature does not verify over ts+METHOD+path (query excluded)")
	}
	if ed25519.Verify(pub, []byte(ts+"GET"+"/v1/orders/open?slugs=abc"), sig) {
		t.Error("signature verifies WITH query params — query leaked into the message")
	}
}

func TestNewSignerAccepts64ByteKey(t *testing.T) {
	// A 64-byte private key is seed || public key; the seed half must yield
	// the same signatures as the seed alone.
	full := ed25519.NewKeyFromSeed(testSeed)
	s64 := testSigner(t, base64.StdEncoding.EncodeToString(full))
	s32 := testSigner(t, base64.StdEncoding.EncodeToString(testSeed))

	_, sig64 := s64.signature("GET", "/v1/account/balances")
	_, sig32 := s32.signature("GET", "/v1/account/balances")
	if sig64 != sig32 {
		t.Error("64-byte and 32-byte secrets sign differently")
	}
}

func TestNewSignerRejectsBadKeys(t *testing.T) {
	if _, err := NewSigner("", base64.StdEncoding.EncodeToString(testSeed)); err == nil {
		t.Error("empty key id accepted")
	}
	if _, err := NewSigner("k", "not!!base64"); err == nil {
		t.Error("non-base64 secret accepted")
	}
	if _, err := NewSigner("k", base64.StdEncoding.EncodeToString(make([]byte, 16))); err == nil {
		t.Error("16-byte secret accepted")
	}
}
