package main

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/smtp"
)

const defaultRelayPort = 587

// outboundRelay builds the optional trusted-relay configuration:
//
//	MAILX_RELAY_HOST      relay host name or IP; setting it enables relay mode
//	MAILX_RELAY_PORT      default 587 (submission with STARTTLS; implicit-TLS
//	                      port 465 is not supported)
//	MAILX_RELAY_USERNAME  SMTP AUTH user; requires MAILX_RELAY_PASSWORD
//	MAILX_RELAY_PASSWORD  SMTP AUTH secret; requires MAILX_RELAY_USERNAME
//
// Unset host means direct MX delivery, which never carries credentials. With
// credentials, TLS is mandatory whatever MAILX_SMTP_TLS_POLICY says, and there
// is no setting that allows AUTH over plaintext. Errors name the variable,
// never a value.
func outboundRelay() (*delivery.Relay, error) {
	host := strings.TrimSpace(os.Getenv("MAILX_RELAY_HOST"))
	portRaw := strings.TrimSpace(os.Getenv("MAILX_RELAY_PORT"))
	user, pass := os.Getenv("MAILX_RELAY_USERNAME"), os.Getenv("MAILX_RELAY_PASSWORD")
	if host == "" {
		if portRaw != "" || user != "" || pass != "" {
			return nil, errors.New("MAILX_RELAY_HOST is required when any other MAILX_RELAY_* setting is set")
		}
		return nil, nil
	}
	if !validRelayHost(host) {
		return nil, errors.New("MAILX_RELAY_HOST is not a valid host name or IP address")
	}
	port := defaultRelayPort
	if portRaw != "" {
		n, err := strconv.Atoi(portRaw)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("MAILX_RELAY_PORT must be a port number between 1 and 65535")
		}
		port = n
	}
	relay := &delivery.Relay{Host: host, Port: port}
	switch {
	case user == "" && pass == "":
	case user == "":
		return nil, errors.New("MAILX_RELAY_PASSWORD is set but MAILX_RELAY_USERNAME is missing")
	case pass == "":
		return nil, errors.New("MAILX_RELAY_USERNAME is set but MAILX_RELAY_PASSWORD is missing")
	default:
		creds := smtp.Credentials{Username: user, Password: pass}
		if err := creds.Validate(); err != nil {
			return nil, errors.New("MAILX_RELAY_USERNAME/MAILX_RELAY_PASSWORD are invalid (length or control characters)")
		}
		relay.Auth = &creds
	}
	return relay, nil
}

func validRelayHost(h string) bool {
	if net.ParseIP(h) != nil {
		return true
	}
	if len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}
