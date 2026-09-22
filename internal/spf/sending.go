package spf

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Mode is how MailX reaches the Internet, mirroring v0.25's routing.
type Mode string

const (
	ModeDirect Mode = "direct" // MailX connects to recipient MX hosts from its own address(es)
	ModeRelay  Mode = "relay"  // every message goes through one trusted relay
)

// MaxSendingIPs bounds MAILX_SENDING_IPS.
const MaxSendingIPs = 16

// Config is MailX's sending infrastructure as the operator declared it. MailX
// cannot discover its public egress address reliably (NAT, proxies, cloud
// gateways), so it never guesses: an unset value yields no instructions.
type Config struct {
	Mode Mode
	// IPs are MailX's public egress addresses (direct mode only).
	IPs []netip.Addr
	// RelayInclude is the include: domain the relay provider documents for SPF
	// (relay mode only), for example the value their onboarding page gives.
	// MailX cannot know it and does not invent one.
	RelayInclude string
}

// Ready reports whether MailX knows enough to give truthful instructions.
func (c Config) Ready() bool {
	switch c.Mode {
	case ModeDirect:
		return len(c.IPs) > 0
	case ModeRelay:
		return c.RelayInclude != ""
	}
	return false
}

// Mechanisms returns the SPF tokens that authorize this configuration.
func (c Config) Mechanisms() []string {
	switch c.Mode {
	case ModeDirect:
		out := make([]string, 0, len(c.IPs))
		for _, ip := range c.IPs {
			if ip.Is4() {
				out = append(out, "ip4:"+ip.String())
			} else {
				out = append(out, "ip6:"+ip.String())
			}
		}
		return out
	case ModeRelay:
		if c.RelayInclude != "" {
			return []string{"include:" + c.RelayInclude}
		}
	}
	return nil
}

// nonPublic lists special-purpose ranges (IANA registries, RFC 6890 and
// successors) that must never be advertised as public sending addresses.
var nonPublic = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16",
	"3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// ErrNonPublicIP marks an address that cannot be a public sending address.
var ErrNonPublicIP = errors.New("not a public unicast address")

// CheckPublicIP rejects loopback, private, link-local, documentation, CGNAT,
// multicast, reserved and unspecified addresses.
func CheckPublicIP(ip netip.Addr) error {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.Zone() != "" {
		return errors.New("not a valid address")
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return ErrNonPublicIP
		}
	}
	if !ip.IsGlobalUnicast() {
		return ErrNonPublicIP
	}
	return nil
}

// ParseSendingIPs parses a comma-separated list of public IPv4/IPv6 addresses
// (MAILX_SENDING_IPS). An empty string means "not declared" and yields nil.
// Errors carry the position and reason, never a parsed value from elsewhere.
func ParseSendingIPs(raw string) ([]netip.Addr, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > MaxSendingIPs {
		return nil, fmt.Errorf("at most %d addresses are allowed", MaxSendingIPs)
	}
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if strings.ContainsRune(p, '/') {
			return nil, fmt.Errorf("entry %d: give single addresses, not CIDR ranges", i+1)
		}
		ip, err := netip.ParseAddr(p)
		if err != nil {
			return nil, fmt.Errorf("entry %d is not an IP address", i+1)
		}
		ip = ip.Unmap()
		if err := CheckPublicIP(ip); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i+1, err)
		}
		if !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out, nil
}
