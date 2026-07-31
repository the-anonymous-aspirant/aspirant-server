package server

import (
	"os"
	"strings"

	"github.com/gin-contrib/cors"
)

// corsAllowedOriginsEnv overrides the default CORS origin allowlist. Its value
// is a comma-separated list of scheme+host origins, e.g.
// "https://the-aspirant.com,http://localhost:5173" for local development.
const corsAllowedOriginsEnv = "CORS_ALLOWED_ORIGINS"

// defaultCORSOrigin is the production web origin. The frontend is served
// same-origin behind nginx at https://the-aspirant.com (see docs/DECISIONS.md,
// "Reverse Proxy Pattern"), so this is the only origin that legitimately makes
// browser calls to /api in production.
const defaultCORSOrigin = "https://the-aspirant.com"

// corsAllowedOrigins returns the CORS origin allowlist. It uses
// CORS_ALLOWED_ORIGINS when set to a non-empty list (comma-separated,
// whitespace-trimmed, blank entries dropped), otherwise the production
// default. It never returns an empty slice, so the middleware always has a
// concrete origin to match and can never fall back to a wildcard.
func corsAllowedOrigins() []string {
	if raw := os.Getenv(corsAllowedOriginsEnv); raw != "" {
		var origins []string
		for _, o := range strings.Split(raw, ",") {
			if t := strings.TrimSpace(o); t != "" {
				origins = append(origins, t)
			}
		}
		if len(origins) > 0 {
			return origins
		}
	}
	return []string{defaultCORSOrigin}
}

// corsConfig builds the CORS middleware configuration. It keeps gin-contrib's
// defaults (allowed methods and headers, 12h preflight cache, credentials
// disabled) but replaces the wildcard origin with an explicit allowlist, so
// Access-Control-Allow-Origin is echoed only for a matched origin — alongside
// Vary: Origin — and disallowed cross-origin requests are rejected. Credentials
// stay disabled; the non-wildcard allowlist is what would make enabling them
// safe later (CWE-942, task #3069).
func corsConfig() cors.Config {
	cfg := cors.DefaultConfig()
	cfg.AllowOrigins = corsAllowedOrigins()
	return cfg
}
