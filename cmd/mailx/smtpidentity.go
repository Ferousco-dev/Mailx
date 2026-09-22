package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/Ferousco-dev/mailx/internal/smtpidentity"
	"github.com/Ferousco-dev/mailx/internal/spf"
)

// smtpID is MailX's SMTP infrastructure identity for this process.
//
//	MAILX_SMTP_HOSTNAME  the public, fully-qualified host name MailX presents in
//	                     EHLO/HELO, uses as the Message-ID domain and as the DSN
//	                     Reporting-MTA. It belongs to whoever operates the sending
//	                     IPs (it is the PTR name), NOT to any tenant domain.
//
// Rules (static validation only; DNS is never consulted at startup, so a DNS
// outage cannot stop MailX from starting):
//   - set: it must be a valid public hostname (smtpidentity.ValidateHostname) or
//     startup fails, naming the variable and a reason code;
//   - MAILX_SENDING_IPS set (the operator declares public direct delivery): the
//     hostname is REQUIRED, so public direct SMTP never identifies as mailx.local;
//   - unset without sending IPs: local/development identity "mailx.local", used
//     for docker compose, tests and fake MX servers. A relay without a hostname
//     also runs on the local identity (logged), because the relay is the visible
//     sender to recipients and PTR belongs to the relay operator.
type smtpID struct {
	Hostname string // validated public hostname, empty in local mode
	Public   bool
}

// Name is the value used for EHLO, Message-ID right-hand side and Reporting-MTA.
func (i smtpID) Name() string {
	if i.Hostname != "" {
		return i.Hostname
	}
	return smtpidentity.LocalIdentity
}

func loadSMTPIdentity() (smtpID, error) {
	raw := os.Getenv("MAILX_SMTP_HOSTNAME")
	ips, _ := spf.ParseSendingIPs(os.Getenv("MAILX_SENDING_IPS")) // a bad list is reported by spfConfig
	if raw == "" {
		if len(ips) > 0 {
			return smtpID{}, errors.New("MAILX_SMTP_HOSTNAME is required when MAILX_SENDING_IPS is set: public direct delivery must not identify as " + smtpidentity.LocalIdentity)
		}
		return smtpID{}, nil
	}
	name, err := smtpidentity.ValidateHostname(raw)
	if err != nil {
		return smtpID{}, fmt.Errorf("MAILX_SMTP_HOSTNAME is invalid: %w", err)
	}
	return smtpID{Hostname: name, Public: true}, nil
}

// cmdCheckSMTPIdentity is the operator diagnostic: it inspects reverse DNS,
// forward confirmation and HELO SPF for the CONFIGURED hostname and sending IPs,
// live, read-only. It is a CLI and not a tenant API on purpose: PTR belongs to the
// operator of the sending IPs, an API that resolved caller-supplied names or
// addresses would be an SSRF-style probe, and health endpoints must not depend on
// public DNS. It exits non-zero unless the identity is ready.
func cmdCheckSMTPIdentity(args []string, output io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: mailx check-smtp-identity (reads MAILX_SMTP_HOSTNAME and MAILX_SENDING_IPS)")
	}
	id, err := loadSMTPIdentity()
	if err != nil {
		return err
	}
	if id.Hostname == "" {
		return errors.New("MAILX_SMTP_HOSTNAME is not set: nothing to check (local identity in use)")
	}
	ips, err := spf.ParseSendingIPs(os.Getenv("MAILX_SENDING_IPS"))
	if err != nil {
		return errors.New("MAILX_SENDING_IPS is invalid: " + err.Error())
	}
	c := &smtpidentity.Checker{Resolver: net.DefaultResolver, LocalAddrs: interfaceAddrs}
	return runSMTPIdentityCheck(context.Background(), output, c, id.Hostname, ips)
}

func runSMTPIdentityCheck(ctx context.Context, out io.Writer, c *smtpidentity.Checker, hostname string, ips []netip.Addr) error {
	rep, err := c.Check(ctx, hostname, ips)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "SMTP hostname: %s\nReadiness: %s\n", rep.Hostname, rep.Readiness)
	for _, r := range rep.IPs {
		fmt.Fprintf(out, "  %s (%s): %s", r.IP, r.Family, r.State)
		if len(r.PTRNames) > 0 {
			fmt.Fprintf(out, "  ptr=%s", strings.Join(r.PTRNames, ","))
		}
		if r.LocalInterface {
			fmt.Fprint(out, "  [assigned to a local interface]")
		}
		fmt.Fprintln(out)
		for _, w := range r.Warnings {
			fmt.Fprintf(out, "    note: %s\n", w)
		}
	}
	fmt.Fprintf(out, "HELO SPF for the hostname: %s\n", rep.HeloSPF.Status)
	if rep.HeloSPF.Status != string(spf.StatusVerified) {
		fmt.Fprintf(out, "  advice (RFC 7208 2.3; also the only SPF identity for bounce mail): publish ONE TXT record at %s: %s\n", rep.Hostname, rep.HeloSPF.Recommended)
	}
	fmt.Fprintln(out, "This is a DNS readiness check only. It does not prove the addresses are this machine's real egress addresses, and it does not guarantee inbox placement.")
	if rep.Readiness != smtpidentity.ReadinessReady {
		return fmt.Errorf("SMTP identity is %s", rep.Readiness)
	}
	return nil
}

func interfaceAddrs() []netip.Addr {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Addr
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out
}
