// Package delivery is the MailX v0.10 synchronous delivery engine.
//
// It combines DNS/MX discovery (internal/dns) with one-hop SMTP transfer
// (internal/transfer), tries MX candidates in preference order per RFC 5321
// §5.1, and returns a structured Result the future retry engine will use.
//
// This package NEVER retries later, NEVER queues work, and NEVER opens a
// second SMTP connection after a message has been accepted by a remote MTA.
//
// Layout:
//
//	delivery.go — Engine, Config, Deliver orchestration, request validation
//	policy.go   — same-operation MX fallback decisions (from RFC + docs)
//	result.go   — Result, Attempt, Error, Kind, IsTemporary
package delivery

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"net"
	"strings"
	"time"

	"github.com/Ferousco-dev/mailx/internal/dns"
	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/transfer"
)

// Resolver is the small DNS boundary the engine consumes. internal/dns's
// *Resolver already satisfies it.
type Resolver interface {
	LookupMX(ctx context.Context, domain string) ([]dns.MX, error)
}

// Transferer is the small transfer boundary. internal/transfer's *Service
// already satisfies it.
type Transferer interface {
	Transfer(ctx context.Context, req transfer.Request) (transfer.Result, error)
}

// Config controls delivery behavior. All fields are read-only after Engine
// construction so Deliver is safe for concurrent use.
type Config struct {
	// SMTPPort is the TCP port MailX dials for MTA-to-MTA SMTP. Default 25.
	// Tests override this to point at a local listener.
	SMTPPort int
	// Shuffle randomizes equal-preference MX groups (RFC 5321 §5.1). If nil,
	// crypto-seeded math/rand/v2 is used. Tests inject a deterministic one.
	Shuffle func([]dns.MX)
	// Relay, when non-nil, routes EVERY delivery through one explicitly
	// configured trusted relay instead of the recipient's MX hosts. There is no
	// MX lookup and no fallback to direct delivery: if the relay fails, the
	// delivery fails (and is retried later) rather than silently going direct.
	// Relay credentials are reachable only through this field, so the direct
	// path can never send them to an MX discovered from DNS.
	Relay *Relay
}

// Relay is a trusted submission relay. Auth may be nil for a relay that
// authenticates by network position; when set, TLS is mandatory before AUTH.
type Relay struct {
	Host string
	Port int
	Auth *smtp.Credentials
}

// DefaultConfig returns the finite production defaults.
func DefaultConfig() Config { return Config{SMTPPort: 25} }

// Request names one delivery operation to one recipient domain.
type Request struct {
	Domain   string
	Envelope mail.Envelope
	Raw      string
}

// Engine performs one synchronous delivery operation per Deliver call.
type Engine struct {
	resolver Resolver
	transfer Transferer
	config   Config
	now      func() time.Time
}

// NewEngine constructs a delivery engine.
func NewEngine(resolver Resolver, transferer Transferer, cfg Config) (*Engine, error) {
	if resolver == nil {
		return nil, errors.New("delivery: resolver must not be nil")
	}
	if transferer == nil {
		return nil, errors.New("delivery: transferer must not be nil")
	}
	if cfg.SMTPPort == 0 {
		cfg.SMTPPort = 25
	}
	if cfg.SMTPPort < 1 || cfg.SMTPPort > 65535 {
		return nil, fmt.Errorf("delivery: SMTP port %d out of range", cfg.SMTPPort)
	}
	if cfg.Shuffle == nil {
		cfg.Shuffle = defaultShuffle
	}
	if cfg.Relay != nil {
		r := *cfg.Relay // private copy: later caller mutation cannot change routing or credentials
		if strings.TrimSpace(r.Host) == "" || strings.ContainsAny(r.Host, " \t\r\n/@") {
			return nil, errors.New("delivery: relay host is invalid")
		}
		if r.Port < 1 || r.Port > 65535 {
			return nil, fmt.Errorf("delivery: relay port %d out of range", r.Port)
		}
		if r.Auth != nil {
			if err := r.Auth.Validate(); err != nil {
				return nil, fmt.Errorf("delivery: relay credentials invalid: %w", err)
			}
			c := *r.Auth
			r.Auth = &c
		}
		cfg.Relay = &r
	}
	return &Engine{
		resolver: resolver,
		transfer: transferer,
		config:   cfg,
		now:      func() time.Time { return time.Now().UTC() },
	}, nil
}

// Deliver performs one delivery operation. It always returns a Result; err
// is non-nil iff Result.Accepted is false. A post-acceptance QUIT failure
// preserves Accepted and returns a nil error (mirroring v0.7/v0.8).
func (e *Engine) Deliver(ctx context.Context, req Request) (Result, error) {
	res := Result{
		DeliveryID: newDeliveryID(),
		Domain:     req.Domain,
		StartedAt:  e.now(),
	}
	// 1) Validate request.
	if err := validateRequest(req); err != nil {
		res.FinishedAt = e.now()
		res.Kind = KindInvalidRequest
		res.FailureMessage = err.Error()
		return res, &Error{Kind: KindInvalidRequest, Domain: req.Domain, Err: err}
	}

	// 2) Routing. Relay mode names the single configured relay and carries the
	// only credentials in the system; direct mode resolves MX hosts and carries
	// none.
	port, auth := e.config.SMTPPort, (*smtp.Credentials)(nil)
	if r := e.config.Relay; r != nil {
		res.Transport = "relay"
		res.MXCandidates = []dns.MX{{Host: r.Host}}
		port, auth = r.Port, r.Auth
	} else {
		res.Transport = "direct"
		candidates, err := e.resolver.LookupMX(ctx, req.Domain)
		if err != nil {
			res.FinishedAt = e.now()
			return finishDNS(res, req.Domain, err)
		}
		res.MXCandidates = orderCandidates(candidates, e.config.Shuffle)
	}

	// 3) Try candidates until acceptance, definitive failure, or exhaustion.
	var lastErr *transfer.TransferError
	var lastRemoteMessage string
	var lastDecision fallbackDecision = stopTemporary
	for _, mx := range res.MXCandidates {
		if err := ctx.Err(); err != nil {
			res.FinishedAt = e.now()
			return finishContext(res, req.Domain, err)
		}
		dest := net.JoinHostPort(mx.Host, fmt.Sprintf("%d", port))
		tRes, tErr := e.transfer.Transfer(ctx, transfer.Request{
			Destination: dest,
			Envelope:    req.Envelope,
			Raw:         req.Raw,
			Auth:        auth,
		})
		att := Attempt{MX: mx, Destination: dest, Transfer: tRes}
		if tRes.Accepted {
			// Terminal success. Preserve QuitError but do NOT try another MX.
			res.Attempts = append(res.Attempts, att)
			res.FinishedAt = e.now()
			res.Kind = KindAccepted
			res.Accepted = true
			res.FinalCode = tRes.FinalCode
			res.RemoteMessage = tRes.RemoteMessage
			res.QuitError = tRes.QuitError
			return res, nil
		}
		var te *transfer.TransferError
		errors.As(tErr, &te)
		att.TransferErr = te
		res.Attempts = append(res.Attempts, att)
		lastErr = te
		// transfer.Result (not TransferError) is where the raw remote SMTP
		// diagnostic text lives on a failed attempt — see transfer.go's
		// Transfer, which sets Result.RemoteMessage from the underlying
		// smtp.DeliveryError.Remote but does not carry it on TransferError.
		lastRemoteMessage = tRes.RemoteMessage
		lastDecision = decideFallback(tErr)
		if lastDecision != tryNext {
			break
		}
	}

	// 4) All candidates exhausted or a definitive stop occurred.
	res.FinishedAt = e.now()
	if err := ctx.Err(); err != nil {
		return finishContext(res, req.Domain, err)
	}
	if len(res.Attempts) == 0 {
		// No candidates existed — treat as DNS failure. This branch is
		// unreachable in the common case because LookupMX would have
		// returned an error already; kept as a defensive fallthrough.
		res.Kind = KindDNSFailure
		res.FailureMessage = "no MX candidates"
		return res, &Error{Kind: KindDNSFailure, Domain: req.Domain, Err: errors.New("no MX candidates")}
	}
	// Failure precedence: if the terminating attempt was stopPermanent, that
	// is permanent. Otherwise (all tryNext exhausted, or stopTemporary), the
	// delivery is temporary. This means a permanent 5xx on any MX beats
	// upstream network failures on earlier candidates — the 5xx is
	// authoritative for the domain.
	kind := classifyFinal(lastDecision, lastErr)
	res.Kind = kind
	if lastErr != nil {
		res.FailureStage = string(lastErr.Stage)
		res.FinalCode = lastErr.Code
		res.EnhancedStatus = lastErr.Enhanced
		res.RemoteMessage = lastRemoteMessage
		res.Recipient = lastErr.Recipient
		res.FailureMessage = lastErr.Error()
	}
	return res, &Error{
		Kind:      kind,
		Domain:    req.Domain,
		Temporary: kind == KindTransferTemporary,
		Err:       lastErr,
	}
}

// validateRequest enforces the delivery contract: non-empty domain, at least
// one recipient, and every recipient envelope address' domain must match the
// requested delivery domain. Mixed-domain recipient sets are rejected to
// prevent misrouting.
func validateRequest(req Request) error {
	if strings.TrimSpace(req.Domain) == "" {
		return errors.New("domain is empty")
	}
	if len(req.Envelope.Recipients) == 0 {
		return errors.New("envelope has no recipients")
	}
	targetDomain := strings.ToLower(strings.TrimSpace(req.Domain))
	for _, r := range req.Envelope.Recipients {
		rd, ok := extractDomain(r)
		if !ok {
			return fmt.Errorf("recipient %q not addressable", r)
		}
		if !strings.EqualFold(rd, targetDomain) {
			return fmt.Errorf("recipient %q domain does not match delivery domain %q", r, targetDomain)
		}
	}
	return nil
}

// extractDomain pulls the domain from a bracketed envelope path like
// "<local@domain>". It never parses full RFC 5322 addresses.
func extractDomain(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(path, "<") || !strings.HasSuffix(path, ">") {
		return "", false
	}
	inner := path[1 : len(path)-1]
	at := strings.LastIndexByte(inner, '@')
	if at < 0 || at == len(inner)-1 {
		return "", false
	}
	return inner[at+1:], true
}

// orderCandidates keeps preference-ascending order (v0.9 already sorts) but
// shuffles equal-preference groups so we don't hammer the same host every
// delivery when the domain publishes multiple hosts at the same preference.
func orderCandidates(in []dns.MX, shuffle func([]dns.MX)) []dns.MX {
	out := append([]dns.MX(nil), in...)
	// walk equal-preference groups
	i := 0
	for i < len(out) {
		j := i + 1
		for j < len(out) && out[j].Preference == out[i].Preference {
			j++
		}
		if j-i > 1 {
			shuffle(out[i:j])
		}
		i = j
	}
	return out
}

func defaultShuffle(mx []dns.MX) {
	mrand.Shuffle(len(mx), func(i, j int) { mx[i], mx[j] = mx[j], mx[i] })
}

func finishDNS(res Result, domain string, err error) (Result, error) {
	var le *dns.LookupError
	if errors.As(err, &le) {
		switch le.Kind {
		case dns.KindInvalidDomain:
			res.Kind = KindInvalidRequest
		case dns.KindNullMX:
			res.Kind = KindDNSNullMX
		case dns.KindNotFound:
			res.Kind = KindDNSNotFound
		case dns.KindTemporary:
			res.Kind = KindDNSTemporary
		default:
			res.Kind = KindDNSFailure
		}
	} else {
		res.Kind = KindDNSFailure
	}
	res.FailureMessage = err.Error()
	return res, &Error{
		Kind:      res.Kind,
		Domain:    domain,
		Temporary: res.Kind == KindDNSTemporary,
		Err:       err,
	}
}

func finishContext(res Result, domain string, ctxErr error) (Result, error) {
	res.Kind = KindContext
	res.FailureMessage = ctxErr.Error()
	return res, &Error{Kind: KindContext, Domain: domain, Temporary: true, Err: ctxErr}
}

func newDeliveryID() string {
	var b [16]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return fmt.Sprintf("delivery-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
