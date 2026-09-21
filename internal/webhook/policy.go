package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type IPResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type URLPolicy struct {
	AllowHTTP    bool
	AllowPrivate bool
	Resolver     IPResolver
	Dialer       *net.Dialer
}

func (p URLPolicy) resolver() IPResolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

func (p URLPolicy) Validate(ctx context.Context, raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "\r\n\x00") {
		return "", errors.New("destination is empty or contains control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("destination must be an absolute URL")
	}
	if u.Scheme != "https" && !(p.AllowHTTP && u.Scheme == "http") {
		return "", errors.New("destination must use HTTPS")
	}
	if u.User != nil || u.Fragment != "" || u.RawFragment != "" {
		return "", errors.New("destination must not contain userinfo or a fragment")
	}
	for _, r := range u.Hostname() {
		if r > 127 || r < 0x21 {
			return "", errors.New("destination hostname must be printable ASCII")
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("destination port is invalid")
		}
	}
	if _, err := p.resolveAndValidate(ctx, u.Hostname()); err != nil {
		return "", err
	}
	return u.String(), nil
}

func (p URLPolicy) resolveAndValidate(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		literal = literal.Unmap()
		if !p.allowed(literal) {
			return nil, errors.New("destination address is not public")
		}
		return []netip.Addr{literal}, nil
	}
	addrs, err := p.resolver().LookupNetIP(ctx, "ip", host)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, errors.New("destination hostname does not resolve")
		}
		// Resolver detail (server addresses, timeouts) stays internal.
		return nil, ErrDNSUnavailable
	}
	if len(addrs) == 0 {
		return nil, errors.New("destination has no IP addresses")
	}
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
		if !p.allowed(addrs[i]) {
			return nil, errors.New("destination resolves to a non-public address")
		}
	}
	return addrs, nil
}

func (p URLPolicy) allowed(ip netip.Addr) bool {
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if p.AllowPrivate {
		return true
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range prohibitedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

var prohibitedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

// DialContext repeats DNS/IP policy at connection time and dials the exact
// validated answer, closing the creation-time DNS-rebinding gap.
func (p URLPolicy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("webhook: malformed dial address: %w", err)
	}
	addrs, err := p.resolveAndValidate(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := p.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	}
	var errs []error
	for _, ip := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}
