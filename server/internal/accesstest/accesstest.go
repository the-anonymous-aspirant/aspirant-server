// Package accesstest stands in for Cloudflare Access in tests: an RSA signing
// key, a local certs endpoint publishing it, and a minter for
// Cf-Access-Jwt-Assertion tokens (system_3 #8798). Imported only by _test
// files, so it never reaches the server binary.
package accesstest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Team is the configured team domain the fixtures use.
const (
	Team       = "aspirators-team.cloudflareaccess.com"
	Issuer     = "https://" + Team
	AUD        = "test-aud-tag"
	AdminEmail = "operator@example.com"
)

// Keys is one signing key, served from a local certs endpoint.
type Keys struct {
	Kid    string
	Key    *rsa.PrivateKey
	Server *httptest.Server
	Status atomic.Int32 // the HTTP status the endpoint answers with; 200 by default
	hits   atomic.Int32
}

// NewKeys starts a certs endpoint publishing a fresh 2048-bit key under kid.
func NewKeys(t *testing.T, kid string) *Keys {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	k := &Keys{Kid: kid, Key: key}
	k.Status.Store(http.StatusOK)
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.hits.Add(1)
		if code := int(k.Status.Load()); code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": kid,
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(k.Server.Close)
	return k
}

// Hits is how many requests the endpoint has answered.
func (k *Keys) Hits() int { return int(k.hits.Load()) }

// URL is the endpoint's certs URL.
func (k *Keys) URL() string { return k.Server.URL + "/cdn-cgi/access/certs" }

// Claims are a valid assertion's claims for the fixture application.
func Claims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":   Issuer,
		"aud":   []string{AUD},
		"email": AdminEmail,
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"sub":   "operator-id",
	}
}

// Mint signs claims with this endpoint's key, under its kid.
func (k *Keys) Mint(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	return MintWith(t, k.Key, k.Kid, claims)
}

// MintWith signs claims with any key, under any kid.
func MintWith(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("minting an assertion: %v", err)
	}
	return signed
}
