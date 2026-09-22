package smtpidentity

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Ferousco-dev/mailx/internal/spf"
)

// Bounds on untrusted DNS and on configuration.
const (
	MaxIPs              = 16 // same as spf.MaxSendingIPs
	MaxPTRNames         = 16
	MaxForwardAddresses = 64
	MaxTXTRecords       = 64
	maxConcurrentPTR    = 4
	defaultCheckTimeout = 10 * time.Second
)

// Resolver is the DNS boundary. *net.Resolver satisfies it; tests inject a fake.
type Resolver interface {
	LookupAddr(ctx context.Context, addr string) ([]string, error)
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// IPState is the reverse/forward DNS state of ONE sending IP.
type IPState string

const (
	IPReady           IPState = "ready"            // PTR names the hostname and the hostname resolves back to the IP
	IPMissingPTR      IPState = "missing_ptr"      // no PTR record
	IPPTRMismatch     IPState = "ptr_mismatch"     // PTR exists but does not name the hostname
	IPForwardMismatch IPState = "forward_mismatch" // PTR names the hostname but it does not resolve back to the IP
	IPMalformedPTR    IPState = "malformed_ptr"    // only unusable PTR names
	IPOversized       IPState = "oversized_dns_answer"
	IPTemporary       IPState = "temporary_error" // DNS failed or timed out: nothing concluded
)

// Readiness aggregates all IPs.
const (
	ReadinessReady         = "ready"
	ReadinessNotReady      = "not_ready"
	ReadinessUnknown       = "unknown"
	ReadinessNotConfigured = "not_configured"
)

// Warning codes.
const (
	WarnMultiplePTR      = "multiple_ptr_records"
	WarnMalformedIgnored = "malformed_ptr_ignored"
	WarnHeloSPFNotReady  = "helo_spf_not_verified"
	WarnNoLocalInterface = "ip_not_on_local_interface"
	WarnTruncatedForward = "forward_answer_truncated"
)

// IPReport is the outcome for one configured sending IP.
type IPReport struct {
	IP       netip.Addr
	Family   string // ipv4 | ipv6
	State    IPState
	PTRNames []string // usable PTR names (bounded, normalized)
	Warnings []string
	// LocalInterface is true when the IP is assigned to a local network
	// interface. It is positive evidence that this host really owns the address;
	// false is NOT evidence against it (cloud hosts sit behind 1:1 NAT), so it is
	// informational only. MailX cannot prove its egress address without a
	// third-party service, which it deliberately never calls (RSK-020).
	LocalInterface bool
}

// HeloSPF is the advisory SPF state of the hostname itself (RFC 7208 2.3: the
// HELO identity is checked separately, and is the only identity for a null
// reverse-path). It never affects Readiness.
type HeloSPF struct {
	Status      string // spf status vocabulary
	Recommended string // the single record to publish at the hostname when missing
}

// Report is one infrastructure readiness assessment.
type Report struct {
	Hostname  string
	Readiness string
	IPs       []IPReport
	HeloSPF   HeloSPF
	Warnings  []string
}

// Checker verifies configured operator infrastructure only. It has no way to
// look up arbitrary names or addresses supplied by anyone else.
type Checker struct {
	Resolver Resolver
	Timeout  time.Duration
	// LocalAddrs lists the host's interface addresses; nil disables the fact.
	LocalAddrs func() []netip.Addr
}

// ErrTooManyIPs is returned when more than MaxIPs are configured.
var ErrTooManyIPs = errors.New("smtpidentity: too many sending IPs")

// Check assesses hostname (already validated) and ips. Per-IP PTR lookups run
// with bounded concurrency; results are ordered by address, so aggregation is
// deterministic. The hostname is resolved once and shared by all IPs.
func (c *Checker) Check(ctx context.Context, hostname string, ips []netip.Addr) (Report, error) {
	rep := Report{Hostname: hostname}
	uniq := dedupe(ips)
	if len(uniq) > MaxIPs {
		return rep, ErrTooManyIPs
	}
	if hostname == "" || len(uniq) == 0 {
		rep.Readiness = ReadinessNotConfigured
		return rep, nil
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	fwd, fwdErr := c.forward(ctx, hostname)
	rep.IPs = make([]IPReport, len(uniq))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentPTR)
	for i, ip := range uniq {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				rep.IPs[i] = IPReport{IP: ip, Family: family(ip), State: IPTemporary}
				return
			}
			rep.IPs[i] = c.checkIP(ctx, hostname, ip, fwd, fwdErr)
		}()
	}
	wg.Wait()
	c.annotateLocal(rep.IPs)
	rep.Readiness = aggregate(rep.IPs)
	// A verdict reached under a canceled or expired context is never trusted, even
	// if a resolver ignored the context and returned answers.
	if ctx.Err() != nil && rep.Readiness == ReadinessReady {
		rep.Readiness = ReadinessUnknown
	}
	rep.HeloSPF = c.heloSPF(ctx, hostname, uniq)
	if rep.HeloSPF.Status != string(spf.StatusVerified) {
		rep.Warnings = append(rep.Warnings, WarnHeloSPFNotReady)
	}
	return rep, nil
}

type forwardResult struct {
	addrs     []netip.Addr
	truncated bool
}

func (c *Checker) forward(ctx context.Context, hostname string) (forwardResult, error) {
	addrs, err := c.Resolver.LookupNetIP(ctx, "ip", hostname)
	if err != nil {
		if notFound(err) {
			return forwardResult{}, nil
		}
		return forwardResult{}, err
	}
	res := forwardResult{}
	for _, a := range addrs {
		if len(res.addrs) == MaxForwardAddresses {
			res.truncated = true
			break
		}
		res.addrs = append(res.addrs, a.Unmap())
	}
	return res, nil
}

func (c *Checker) checkIP(ctx context.Context, hostname string, ip netip.Addr, fwd forwardResult, fwdErr error) IPReport {
	r := IPReport{IP: ip, Family: family(ip)}
	names, err := c.Resolver.LookupAddr(ctx, ip.String())
	switch {
	case err != nil && !notFound(err):
		r.State = IPTemporary // SERVFAIL, timeout, cancellation: never a verdict
		return r
	case err != nil || len(names) == 0:
		r.State = IPMissingPTR
		return r
	case len(names) > MaxPTRNames:
		r.State = IPOversized
		return r
	}
	malformed := 0
	seen := map[string]bool{}
	for _, n := range names {
		name, ok := normalizePTR(n)
		if !ok {
			malformed++
			continue
		}
		if !seen[name] {
			seen[name] = true
			r.PTRNames = append(r.PTRNames, name)
		}
	}
	sort.Strings(r.PTRNames)
	switch {
	case len(r.PTRNames) == 0:
		r.State = IPMalformedPTR
		return r
	case !seen[hostname]:
		r.State = IPPTRMismatch // the PTR names some other host: EHLO and PTR are unrelated
		return r
	}
	// The PTR names our hostname: confirm it resolves back to this very address.
	// Membership, not equality: a host with several addresses is normal.
	if fwdErr != nil {
		r.State = IPTemporary
		return r
	}
	contains := false
	for _, a := range fwd.addrs {
		if a == ip {
			contains = true
			break
		}
	}
	if !contains {
		r.State = IPForwardMismatch
		if fwd.truncated {
			r.State, r.Warnings = IPOversized, append(r.Warnings, WarnTruncatedForward)
		}
		return r
	}
	r.State = IPReady
	if len(r.PTRNames) > 1 {
		r.Warnings = append(r.Warnings, WarnMultiplePTR)
	}
	if malformed > 0 {
		r.Warnings = append(r.Warnings, WarnMalformedIgnored)
	}
	return r
}

// aggregate: any definite problem makes the set not ready; otherwise any
// uncertainty makes it unknown; only all-IPs-ready is ready. One healthy IP never
// hides a broken one.
func aggregate(ips []IPReport) string {
	unknown := false
	for _, r := range ips {
		switch r.State {
		case IPReady:
		case IPTemporary:
			unknown = true
		default:
			return ReadinessNotReady
		}
	}
	if unknown {
		return ReadinessUnknown
	}
	return ReadinessReady
}

func (c *Checker) annotateLocal(ips []IPReport) {
	if c.LocalAddrs == nil {
		return
	}
	local := map[netip.Addr]bool{}
	for _, a := range c.LocalAddrs() {
		local[a.Unmap()] = true
	}
	for i := range ips {
		ips[i].LocalInterface = local[ips[i].IP]
		if !ips[i].LocalInterface {
			ips[i].Warnings = append(ips[i].Warnings, WarnNoLocalInterface)
		}
	}
}

// heloSPF reuses the SPF analysis with the hostname as the domain. The
// recommendation is a single restrictive record: the host is only ever used by
// MailX, so -all is appropriate (unlike a tenant domain, where ~all is the
// conservative default).
func (c *Checker) heloSPF(ctx context.Context, hostname string, ips []netip.Addr) HeloSPF {
	cfg := spf.Config{Mode: spf.ModeDirect, IPs: ips}
	recs, err := c.Resolver.LookupTXT(ctx, hostname)
	res := spf.Analyze(cfg, hostname, recs, err)
	return HeloSPF{Status: string(res.Status), Recommended: "v=spf1 " + strings.Join(cfg.Mechanisms(), " ") + " -all"}
}

func family(ip netip.Addr) string {
	if ip.Is4() {
		return "ipv4"
	}
	return "ipv6"
}

func dedupe(ips []netip.Addr) []netip.Addr {
	seen := map[netip.Addr]bool{}
	var out []netip.Addr
	for _, ip := range ips {
		ip = ip.Unmap()
		if ip.IsValid() && !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Less(out[j]) })
	return out
}

// normalizePTR lower-cases, removes one trailing root dot and requires a valid
// multi-label ASCII host name (RFC 1912 2.1). Malformed or hostile PTR content is
// dropped here and never reaches logs or reports.
func normalizePTR(n string) (string, bool) {
	name := strings.ToLower(strings.TrimSuffix(n, "."))
	if name == "" || len(name) > MaxHostnameBytes {
		return "", false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return "", false
	}
	for _, l := range labels {
		if !validHostLabel(l) {
			return "", false
		}
	}
	return name, true
}

// notFound reports "no such record" (NXDOMAIN or empty answer), which is a
// definite absence, unlike timeouts and server failures.
func notFound(err error) bool {
	var d *net.DNSError
	return errors.As(err, &d) && d.IsNotFound && !d.IsTemporary && !d.IsTimeout
}
