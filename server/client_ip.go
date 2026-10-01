package server

import "net"

// Access-log client-IP masking (system_3 #5991).
//
// The Gin access log carried the full client IP (`param.ClientIP`). On a
// publicly reachable site that makes every log line hold personal data, which
// is what the operator asked about on #5991: "If there is no law being violated
// by this, we can keep an IP adress log ... But if it would cause potential
// legal issues, we don't want to do it." Masking the host portion keeps what
// the field is actually used for -- telling "same visitor" and "same network"
// apart while debugging or spotting abuse -- without retaining an address that
// identifies a person.
//
// This is deliberately scoped to the access log. Epic #5113's NO-IP-RETENTION
// constraint already bars an IP from a user row, a moderation record, or an
// over-limit log line; nothing here widens the access log into any of those.

const (
	// clientIPUnknown replaces a ClientIP that does not parse as an address.
	// The raw value is dropped rather than echoed: an unparsable value is
	// attacker-supplied as often as not, and echoing it would reintroduce
	// exactly the retention this masking removes.
	clientIPUnknown = "unknown"

	// maskedIPv4PrefixBits keeps the /24 -- the "mask the last octet" the
	// operator's decision names.
	maskedIPv4PrefixBits = 24

	// maskedIPv6PrefixBits keeps the /64. IPv6 has no "last octet" to drop;
	// /64 is the network boundary a single subscriber is assigned, so it is
	// the IPv4 /24's analogue: the network stays legible, the host identifier
	// within it does not.
	maskedIPv6PrefixBits = 64
)

// maskClientIP reduces a client IP to its network portion for logging.
//
// Addresses that cannot identify a person are returned verbatim, because they
// are the ones carrying operational meaning and they carry no privacy cost:
// loopback (the 30s container health check is the bulk of this log), private
// and unique-local ranges, link-local, and the unspecified address. Every
// globally routable address is masked.
func maskClientIP(raw string) string {
	ip := net.ParseIP(raw)
	if ip == nil {
		return clientIPUnknown
	}
	if !isGloballyRoutable(ip) {
		return raw
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(maskedIPv4PrefixBits, 32)).String()
	}
	return ip.Mask(net.CIDRMask(maskedIPv6PrefixBits, 128)).String()
}

// isGloballyRoutable reports whether ip is an address that could identify a
// visitor, as opposed to infrastructure the deployment talks to itself.
func isGloballyRoutable(ip net.IP) bool {
	return !ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsUnspecified() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast()
}
