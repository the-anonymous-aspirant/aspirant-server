package handlers

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// Social links on a client and on the issuer profile (system_3 #9971,
// operator c61586). Flower types a handle or pastes a link; the server stores
// one canonical https URL on the platform's own host, or refuses. What is
// stored is later rendered as a link in the app, so nothing but these two
// shapes may ever reach the column: no other scheme (javascript:, data:), no
// other host, no userinfo, no port, no path beyond the profile's own.

// maxSocialInputLen bounds what is parsed at all.
const maxSocialInputLen = 200

var errSocialLink = errors.New("not a profile link or handle")

// handleWordChar: a handle must hold at least one letter, digit or
// underscore, so dots alone ("." or "..") are not a profile (security's nit
// on PR 166, system_3 #10019). Telegram's own pattern already starts with a
// letter; the check is shared so neither platform can store a dot path.
var handleWordChar = regexp.MustCompile(`[A-Za-z0-9_]`)

type socialPlatform struct {
	hosts  map[string]bool
	handle *regexp.Regexp
	// canonical renders the stored URL from a valid handle.
	canonical func(handle string) string
}

var (
	instagramPlatform = socialPlatform{
		hosts:  map[string]bool{"instagram.com": true, "www.instagram.com": true, "m.instagram.com": true},
		handle: regexp.MustCompile(`^[A-Za-z0-9._]{1,30}$`),
		// Instagram handles are case-insensitive and shown lowercase.
		canonical: func(h string) string { return "https://www.instagram.com/" + strings.ToLower(h) + "/" },
	}
	telegramPlatform = socialPlatform{
		hosts: map[string]bool{"t.me": true, "telegram.me": true, "www.telegram.me": true},
		// A Telegram username starts with a letter: letters, digits and
		// underscores, 4-32 in all (4 for the short collectible names).
		handle:    regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`),
		canonical: func(h string) string { return "https://t.me/" + h },
	}
)

// normalize returns the canonical URL for a handle or profile link, "" for
// empty input, or errSocialLink.
func (p socialPlatform) normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if len(s) > maxSocialInputLen {
		return "", errSocialLink
	}
	handle := s
	if strings.Contains(s, "/") || strings.Contains(s, ":") {
		// A link. Without a scheme ("instagram.com/name") it is read as https.
		if !strings.Contains(s, "://") {
			if strings.Contains(s, ":") {
				return "", errSocialLink // javascript:..., data:..., host:port
			}
			s = "https://" + s
		}
		u, err := url.Parse(s)
		if err != nil {
			return "", errSocialLink
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "https" && scheme != "http" {
			return "", errSocialLink
		}
		if u.User != nil || u.Port() != "" || u.Opaque != "" || !p.hosts[strings.ToLower(u.Hostname())] {
			return "", errSocialLink
		}
		// Exactly one path segment, an optional trailing slash: a profile,
		// not a post or anything else on the platform.
		path := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
		if path == "" || strings.Contains(path, "/") {
			return "", errSocialLink
		}
		handle = path
	}
	handle = strings.TrimPrefix(handle, "@")
	if !p.handle.MatchString(handle) || !handleWordChar.MatchString(handle) {
		return "", errSocialLink
	}
	return p.canonical(handle), nil
}
