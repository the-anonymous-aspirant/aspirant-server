package middleware

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Cloudflare Access gate for the Admin tier (system_3 #8798, option D).
//
// The operator's second factor is Cloudflare Access One-time PIN on /admin and
// /browser-flows. Access only guards what passes through Cloudflare, and the
// admin API is served under /api, which has to stay public for sign-up, member
// login and the games. So the Admin tier is issued at the origin, on one sign-in
// path, and only to a request that proves it passed Access: a valid
// Cf-Access-Jwt-Assertion for the /admin application. The check runs here, not
// at the edge, so a caller that reaches the server around Cloudflare (:8081, a
// sibling container) gets no Admin token either.
//
// The one exception is break-glass: the loopback-only nginx listener sets
// X-Aspirant-Break-Glass to a secret, for when Cloudflare is down or locks the
// operator out. Every public nginx location clears that header. The password is
// required on both paths.
const (
	AccessTeamDomainEnv = "CF_ACCESS_TEAM_DOMAIN"
	AccessAUDEnv        = "CF_ACCESS_AUD"
	AccessAdminEmailEnv = "CF_ACCESS_ADMIN_EMAIL"
	BreakGlassTokenEnv  = "BREAK_GLASS_TOKEN"

	AccessAssertionHeader = "Cf-Access-Jwt-Assertion"
	BreakGlassHeader      = "X-Aspirant-Break-Glass"

	accessLeeway           = 30 * time.Second
	accessKeysTTL          = time.Hour
	accessKeysMinRefetch   = time.Minute
	accessCertsHTTPTimeout = 5 * time.Second
)

// AccessMode says how the Admin tier is gated. It is read from the
// environment on every call, like the rest of the server's configuration.
type AccessMode int

const (
	// AccessOff: none of the three CF_ACCESS_* values is set. Today's
	// behaviour: the password alone issues Admin, on /login and /login/admin.
	// This is what lets the server change merge before the Access
	// applications exist without locking the operator out.
	AccessOff AccessMode = iota
	// AccessEnforced: all three are set. Admin only via /login/admin with a
	// valid assertion or the break-glass token.
	AccessEnforced
	// AccessMisconfigured: some but not all are set. Fails closed: only
	// break-glass issues Admin.
	AccessMisconfigured
)

// String is the mode's name on the health response and in logs. The security
// sweep reads it there and raises a finding whenever it is not "enforced"
// (security c57514 on system_3 #8798), so a lost .env value cannot quietly
// return the Admin tier to password-only.
func (m AccessMode) String() string {
	switch m {
	case AccessOff:
		return "off"
	case AccessEnforced:
		return "enforced"
	default:
		return "misconfigured"
	}
}

// teamDomainShape is a bare host name: no scheme, port, path or userinfo, so
// the configured value can only ever name a host under the issuer we build.
var teamDomainShape = regexp.MustCompile(`^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)

// AccessConfig is the /admin Access application the assertion must come from.
type AccessConfig struct {
	TeamDomain string // e.g. "aspirators-team.cloudflareaccess.com"
	AUD        string // the application's AUD tag
	AdminEmail string // the one address the policy allows
}

// LoadAccessConfig reads the CF_ACCESS_* values and the mode they put the
// server in.
func LoadAccessConfig() (AccessConfig, AccessMode) {
	cfg := AccessConfig{
		TeamDomain: strings.TrimSpace(os.Getenv(AccessTeamDomainEnv)),
		AUD:        strings.TrimSpace(os.Getenv(AccessAUDEnv)),
		AdminEmail: strings.TrimSpace(os.Getenv(AccessAdminEmailEnv)),
	}
	set := 0
	for _, v := range []string{cfg.TeamDomain, cfg.AUD, cfg.AdminEmail} {
		if v != "" {
			set++
		}
	}
	switch set {
	case 0:
		return cfg, AccessOff
	case 3:
		if !teamDomainShape.MatchString(cfg.TeamDomain) {
			return cfg, AccessMisconfigured
		}
		return cfg, AccessEnforced
	default:
		return cfg, AccessMisconfigured
	}
}

// Issuer is the iss claim Cloudflare stamps on the team's assertions, built
// from configuration only (the team domain is a bare host, teamDomainShape).
func (cfg AccessConfig) Issuer() string {
	return "https://" + cfg.TeamDomain
}

// AccessCertsURL maps the configured issuer to its signing-key endpoint. It is
// never fed anything from the token: a URL taken from a caller-written iss
// would have the server fetch an address of the caller's choosing before
// anything in the token is trusted (security c57526). A variable so tests can
// point it at a local server.
var AccessCertsURL = func(issuer string) string {
	return issuer + "/cdn-cgi/access/certs"
}

type accessKeyCache struct {
	mu        sync.Mutex
	url       string
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	client    *http.Client
	nowFn     func() time.Time
}

var defaultAccessKeys = &accessKeyCache{
	client: &http.Client{Timeout: accessCertsHTTPTimeout},
	nowFn:  time.Now,
}

// key returns the signing key for kid, fetching the team's keys when the cache
// is empty, stale, or for another team, and refetching on an unknown kid at
// most once a minute (Cloudflare rotates keys and publishes the next one
// ahead).
func (k *accessKeyCache) key(url, kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.nowFn()
	fresh := k.url == url && k.keys != nil && now.Sub(k.fetchedAt) < accessKeysTTL
	if key, ok := k.keys[kid]; fresh && ok {
		return key, nil
	}
	if fresh && now.Sub(k.fetchedAt) < accessKeysMinRefetch {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	keys, err := fetchAccessKeys(k.client, url)
	if err != nil {
		// A cached key set for this team keeps serving until a refetch
		// succeeds, so a Cloudflare blip does not lock the operator out. With
		// nothing cached, refuse (security c57526).
		if key, ok := k.keys[kid]; ok && k.url == url {
			log.Printf("Access signing keys: refetch failed, serving the cached set: %v", err)
			return key, nil
		}
		return nil, err
	}
	k.url, k.keys, k.fetchedAt = url, keys, now
	if key, ok := keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func fetchAccessKeys(client *http.Client, url string) (map[string]*rsa.PublicKey, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching Access signing keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching Access signing keys: http %d", resp.StatusCode)
	}
	var body struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decoding Access signing keys: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(body.Keys))
	for _, jwk := range body.Keys {
		if jwk.Kty != "RSA" || jwk.Kid == "" {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(jwk.N)
		e, errE := base64.RawURLEncoding.DecodeString(jwk.E)
		if errN != nil || errE != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		keys[jwk.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(keys) == 0 {
		return nil, errors.New("the Access certs endpoint returned no RSA keys")
	}
	return keys, nil
}

// VerifyAccessAssertion checks a Cf-Access-Jwt-Assertion against the /admin
// application: RS256 by one of the team's current keys, aud the application's
// tag, iss the team, unexpired, and email the admin address (case-insensitive).
func VerifyAccessAssertion(assertion string, cfg AccessConfig) error {
	if assertion == "" {
		return errors.New("no Access assertion")
	}
	issuer := cfg.Issuer()
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithAudience(cfg.AUD),
		jwt.WithIssuer(issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(accessLeeway),
	)
	claims := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(assertion, claims, func(t *jwt.Token) (interface{}, error) {
		// Second layer under WithValidMethods: an HMAC token keyed with the
		// RSA public key must never reach a verifier.
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		return defaultAccessKeys.key(AccessCertsURL(issuer), kid)
	})
	if err != nil {
		return err
	}
	email, _ := claims["email"].(string)
	if email == "" || !strings.EqualFold(email, cfg.AdminEmail) {
		return errors.New("the assertion's email is not the admin address")
	}
	return nil
}

// BreakGlassValid reports whether presented is the configured break-glass
// token. An unset token never matches, so an empty header cannot pass. Both
// sides are hashed first so the constant-time compare runs on equal-length
// input and the token's length is not observable (security c57514).
func BreakGlassValid(presented string) bool {
	token := os.Getenv(BreakGlassTokenEnv)
	if token == "" || presented == "" {
		return false
	}
	want, got := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
