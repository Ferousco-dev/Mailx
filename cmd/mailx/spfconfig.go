package main

import (
	"errors"
	"net"
	"os"
	"strings"

	"github.com/Ferousco-dev/mailx/internal/database"
	maildomain "github.com/Ferousco-dev/mailx/internal/domain"
	"github.com/Ferousco-dev/mailx/internal/spf"
)

// spfConfig reads the operator's declaration of MailX's sending infrastructure:
//
//	MAILX_SENDING_IPS        direct mode: comma-separated PUBLIC IPv4/IPv6
//	                         addresses MailX connects from (its egress addresses)
//	MAILX_SPF_RELAY_INCLUDE  relay mode: the SPF include domain the relay
//	                         provider documents
//
// Both are optional. Unset means "not declared": the SPF endpoints then say so
// and MailX invents nothing. Loopback, private, link-local, CGNAT, documentation
// and other non-public addresses are rejected at startup so they can never be
// advertised as production authorization. The two settings are mutually
// exclusive with the routing mode, because an SPF record for infrastructure
// MailX does not use would be a false statement. SPF is guidance only: nothing
// here affects delivery, routing, TLS, AUTH, DKIM or authorization.
func spfConfig(relayMode bool) (spf.Config, error) {
	ips, err := spf.ParseSendingIPs(os.Getenv("MAILX_SENDING_IPS"))
	if err != nil {
		return spf.Config{}, errors.New("MAILX_SENDING_IPS is invalid: " + err.Error())
	}
	include := strings.TrimSuffix(strings.TrimSpace(os.Getenv("MAILX_SPF_RELAY_INCLUDE")), ".")
	if relayMode {
		if len(ips) > 0 {
			return spf.Config{}, errors.New("MAILX_SENDING_IPS must not be set in relay mode (MAILX_RELAY_HOST): MailX does not send from its own addresses then")
		}
		if include != "" && (net.ParseIP(include) != nil || !validRelayHost(include) || strings.ContainsAny(include, " \t")) {
			return spf.Config{}, errors.New("MAILX_SPF_RELAY_INCLUDE must be a host name")
		}
		return spf.Config{Mode: spf.ModeRelay, RelayInclude: strings.ToLower(include)}, nil
	}
	if include != "" {
		return spf.Config{}, errors.New("MAILX_SPF_RELAY_INCLUDE requires relay mode (MAILX_RELAY_HOST)")
	}
	return spf.Config{Mode: spf.ModeDirect, IPs: ips}, nil
}

func buildSPF(db *database.DB, o obs) (*spf.Service, error) {
	relay, err := outboundRelay()
	if err != nil {
		return nil, err
	}
	cfg, err := spfConfig(relay != nil)
	if err != nil {
		return nil, err
	}
	var observer spf.Observer
	if o.metrics != nil {
		observer = o.metrics
	}
	return spf.NewService(db, maildomain.NewNetTXTResolver(), cfg, observer)
}
