package server

import (
	"fmt"
	"net"
)

// TrustedProxyCIDRs reports the subnet(s) of this process's own non-loopback
// IPv4 interfaces -- the docker compose network aspirant-server is attached
// to (system_3 #8822 / #8802). Gin's default TrustedProxies is 0.0.0.0/0
// ("trust every proxy"), so with no SetTrustedProxies call c.ClientIP()
// honours whatever X-Forwarded-For a caller sends, including a caller that
// never passed through nginx. Restricting trust to this subnet means the
// header is only honoured from a peer inside the compose network (nginx);
// a direct caller (e.g. the published :8081 port) gets its own real peer
// address instead, with any X-Forwarded-For it sent ignored.
//
// The subnet is read from the live interface rather than hard-coded: the
// compose "default" network's subnet is auto-assigned by the docker bridge
// driver (docker-compose.yml declares no fixed ipam block for it) and can
// change across a docker-compose down/up cycle.
func TrustedProxyCIDRs() ([]string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("listing network interfaces: %w", err)
	}

	var cidrs []string
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.To4() == nil {
			continue
		}
		network := net.IPNet{IP: ipNet.IP.Mask(ipNet.Mask), Mask: ipNet.Mask}
		cidrs = append(cidrs, network.String())
	}

	if len(cidrs) == 0 {
		return nil, fmt.Errorf("no non-loopback IPv4 interface found to derive a trusted proxy CIDR from")
	}
	return cidrs, nil
}
