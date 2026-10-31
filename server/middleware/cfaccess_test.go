package middleware

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"strings"
	"testing"
	"time"

	"aspirant-online/server/internal/accesstest"

	"github.com/golang-jwt/jwt/v5"
)

var fixtureConfig = AccessConfig{
	TeamDomain: accesstest.Team,
	AUD:        accesstest.AUD,
	AdminEmail: accesstest.AdminEmail,
}

// useKeys points the verifier at k's endpoint, whatever issuer it is handed,
// and gives it an empty key cache with a controllable clock. The issuers it
// was asked for are recorded so a test can check none came from a token.
func useKeys(t *testing.T, k *accesstest.Keys) (asked *[]string, now *time.Time) {
	t.Helper()
	var issuers []string
	clock := time.Now()
	prevURL, prevCache := AccessCertsURL, defaultAccessKeys
	AccessCertsURL = func(issuer string) string {
		issuers = append(issuers, issuer)
		return k.URL()
	}
	defaultAccessKeys = &accessKeyCache{client: k.Server.Client(), nowFn: func() time.Time { return clock }}
	t.Cleanup(func() { AccessCertsURL, defaultAccessKeys = prevURL, prevCache })
	return &issuers, &clock
}

func TestVerifyAccessAssertionAcceptsAValidAssertion(t *testing.T) {
	k := accesstest.NewKeys(t, "kid-1")
	useKeys(t, k)
	if err := VerifyAccessAssertion(k.Mint(t, accesstest.Claims()), fixtureConfig); err != nil {
		t.Fatalf("valid assertion refused: %v", err)
	}
}

func TestVerifyAccessAssertionRefuses(t *testing.T) {
	k := accesstest.NewKeys(t, "kid-1")
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	with := func(edit func(jwt.MapClaims)) jwt.MapClaims {
		c := accesstest.Claims()
		edit(c)
		return c
	}
	cases := map[string]func(t *testing.T) string{
		"empty": func(t *testing.T) string { return "" },
		"wrong aud": func(t *testing.T) string {
			return k.Mint(t, with(func(c jwt.MapClaims) { c["aud"] = []string{"other-app"} }))
		},
		"no aud": func(t *testing.T) string { return k.Mint(t, with(func(c jwt.MapClaims) { delete(c, "aud") })) },
		"wrong iss": func(t *testing.T) string {
			return k.Mint(t, with(func(c jwt.MapClaims) { c["iss"] = "https://evil.cloudflareaccess.com" }))
		},
		"expired": func(t *testing.T) string {
			return k.Mint(t, with(func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }))
		},
		"no exp": func(t *testing.T) string { return k.Mint(t, with(func(c jwt.MapClaims) { delete(c, "exp") })) },
		"not yet valid": func(t *testing.T) string {
			return k.Mint(t, with(func(c jwt.MapClaims) { c["nbf"] = time.Now().Add(time.Hour).Unix() }))
		},
		"wrong email": func(t *testing.T) string {
			return k.Mint(t, with(func(c jwt.MapClaims) { c["email"] = "someone@example.com" }))
		},
		"no email":    func(t *testing.T) string { return k.Mint(t, with(func(c jwt.MapClaims) { delete(c, "email") })) },
		"empty email": func(t *testing.T) string { return k.Mint(t, with(func(c jwt.MapClaims) { c["email"] = "" })) },
		"another key": func(t *testing.T) string { return accesstest.MintWith(t, other, "kid-1", accesstest.Claims()) },
		"unknown kid": func(t *testing.T) string { return accesstest.MintWith(t, k.Key, "kid-2", accesstest.Claims()) },
		"HS256 keyed with the public modulus": func(t *testing.T) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, accesstest.Claims())
			tok.Header["kid"] = "kid-1"
			s, err := tok.SignedString(k.Key.N.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
		"alg none": func(t *testing.T) string {
			tok := jwt.NewWithClaims(jwt.SigningMethodNone, accesstest.Claims())
			tok.Header["kid"] = "kid-1"
			s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
	for name, mint := range cases {
		t.Run(name, func(t *testing.T) {
			useKeys(t, k)
			if err := VerifyAccessAssertion(mint(t), fixtureConfig); err == nil {
				t.Fatalf("%s: accepted, want refused", name)
			}
		})
	}
}

// Security c57526: the certs URL comes from configuration, never from the
// token. A token naming another host as its issuer must not make the server
// fetch from that host.
func TestVerifyAccessAssertionNeverFetchesFromTheTokensIssuer(t *testing.T) {
	k := accesstest.NewKeys(t, "kid-1")
	attacker := accesstest.NewKeys(t, "kid-1")
	asked, _ := useKeys(t, k)
	claims := accesstest.Claims()
	claims["iss"] = attacker.Server.URL

	if err := VerifyAccessAssertion(attacker.Mint(t, claims), fixtureConfig); err == nil {
		t.Fatal("assertion with a foreign iss accepted")
	}
	if attacker.Hits() != 0 {
		t.Fatalf("the token's issuer was fetched %d time(s)", attacker.Hits())
	}
	for _, issuer := range *asked {
		if issuer != accesstest.Issuer {
			t.Fatalf("certs URL built from %q, want only the configured %q", issuer, accesstest.Issuer)
		}
	}
}

func TestVerifyAccessAssertionAllowsSmallClockSkew(t *testing.T) {
	k := accesstest.NewKeys(t, "kid-1")
	useKeys(t, k)
	claims := accesstest.Claims()
	claims["exp"] = time.Now().Add(-10 * time.Second).Unix()
	if err := VerifyAccessAssertion(k.Mint(t, claims), fixtureConfig); err != nil {
		t.Fatalf("assertion 10s past exp refused inside the 30s leeway: %v", err)
	}
}

func TestVerifyAccessAssertionComparesEmailIgnoringCase(t *testing.T) {
	k := accesstest.NewKeys(t, "kid-1")
	useKeys(t, k)
	claims := accesstest.Claims()
	claims["email"] = strings.ToUpper(accesstest.AdminEmail)
	if err := VerifyAccessAssertion(k.Mint(t, claims), fixtureConfig); err != nil {
		t.Fatalf("email differing only in case refused: %v", err)
	}
}

func TestAccessKeysUnknownKidRefetchesAtMostOncePerMinute(t *testing.T) {
	k := accesstest.NewKeys(t, "kid-1")
	_, now := useKeys(t, k)
	stranger := accesstest.MintWith(t, k.Key, "kid-2", accesstest.Claims())

	_ = VerifyAccessAssertion(k.Mint(t, accesstest.Claims()), fixtureConfig) // warm: 1 fetch
	_ = VerifyAccessAssertion(stranger, fixtureConfig)                       // inside the minute: no refetch
	if got := k.Hits(); got != 1 {
		t.Fatalf("fetches after an unknown kid inside a minute = %d, want 1", got)
	}
	*now = now.Add(2 * time.Minute)
	_ = VerifyAccessAssertion(stranger, fixtureConfig)
	if got := k.Hits(); got != 2 {
		t.Fatalf("fetches after an unknown kid past the minute = %d, want 2", got)
	}
}

func TestAccessKeysFetchFailure(t *testing.T) {
	t.Run("empty cache refuses", func(t *testing.T) {
		k := accesstest.NewKeys(t, "kid-1")
		useKeys(t, k)
		k.Status.Store(http.StatusInternalServerError)
		if err := VerifyAccessAssertion(k.Mint(t, accesstest.Claims()), fixtureConfig); err == nil {
			t.Fatal("accepted with no keys fetched")
		}
	})
	t.Run("a cached set keeps serving until a refetch succeeds", func(t *testing.T) {
		k := accesstest.NewKeys(t, "kid-1")
		_, now := useKeys(t, k)
		token := k.Mint(t, accesstest.Claims())
		if err := VerifyAccessAssertion(token, fixtureConfig); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
		k.Status.Store(http.StatusBadGateway)
		*now = now.Add(2 * accessKeysTTL)
		if err := VerifyAccessAssertion(token, fixtureConfig); err != nil {
			t.Fatalf("stale cache refused during an outage: %v", err)
		}
		if k.Hits() != 2 {
			t.Fatalf("fetches = %d, want 2 (the warm-up and the failed refetch)", k.Hits())
		}
	})
}

func TestLoadAccessConfigModes(t *testing.T) {
	cases := []struct {
		name             string
		team, aud, email string
		want             AccessMode
		wantName         string
	}{
		{"none set", "", "", "", AccessOff, "off"},
		{"all set", accesstest.Team, accesstest.AUD, accesstest.AdminEmail, AccessEnforced, "enforced"},
		{"team missing", "", accesstest.AUD, accesstest.AdminEmail, AccessMisconfigured, "misconfigured"},
		{"only aud", "", accesstest.AUD, "", AccessMisconfigured, "misconfigured"},
		{"team with a scheme", "https://" + accesstest.Team, accesstest.AUD, accesstest.AdminEmail, AccessMisconfigured, "misconfigured"},
		{"team with a path", accesstest.Team + "/x", accesstest.AUD, accesstest.AdminEmail, AccessMisconfigured, "misconfigured"},
		{"team with a port", accesstest.Team + ":8443", accesstest.AUD, accesstest.AdminEmail, AccessMisconfigured, "misconfigured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(AccessTeamDomainEnv, tc.team)
			t.Setenv(AccessAUDEnv, tc.aud)
			t.Setenv(AccessAdminEmailEnv, tc.email)
			_, mode := LoadAccessConfig()
			if mode != tc.want || mode.String() != tc.wantName {
				t.Fatalf("mode = %v (%d), want %s", mode, mode, tc.wantName)
			}
		})
	}
}

func TestBreakGlassValid(t *testing.T) {
	t.Setenv(BreakGlassTokenEnv, "")
	if BreakGlassValid("") || BreakGlassValid("anything") {
		t.Fatal("break-glass matched with no token configured")
	}
	t.Setenv(BreakGlassTokenEnv, "a-long-break-glass-token-for-the-tests")
	if !BreakGlassValid("a-long-break-glass-token-for-the-tests") {
		t.Fatal("the configured token did not match")
	}
	for _, wrong := range []string{"", "a-long-break-glass-token-for-the-test", "x"} {
		if BreakGlassValid(wrong) {
			t.Fatalf("%q matched", wrong)
		}
	}
}
