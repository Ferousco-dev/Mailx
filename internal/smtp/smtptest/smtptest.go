// Package smtptest provides a scriptable fake SMTP/STARTTLS server and an
// ephemeral CA for tests. It exists so client, delivery and worker tests can
// exercise real TLS deterministically. Keys live only in memory for the test
// process; nothing is written to disk. Not for production use.
package smtptest

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// PKI is an ephemeral CA plus helpers to mint leaf certificates.
type PKI struct {
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	Pool   *x509.CertPool
	serial int64
}

// NewPKI creates an ephemeral CA.
func NewPKI(t testing.TB) *PKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mailx test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &PKI{ca: ca, caKey: key, Pool: pool, serial: 1}
}

// leaf mints a server certificate signed by this CA.
// Leaf mints a server certificate signed by this CA (notAfter is relative to now).
func (p *PKI) Leaf(t testing.TB, dns []string, ips []net.IP, notAfter time.Duration) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p.serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: "mx.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(notAfter),
		DNSNames: dns, IPAddresses: ips, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// HandshakeMode scripts what the server does after replying 220 to STARTTLS.
type HandshakeMode int

const (
	HSOK      HandshakeMode = iota
	HSClose                 // close the connection right after the 220
	HSStall                 // read the ClientHello, never answer
	HSGarbage               // answer the ClientHello with non-TLS bytes
)

// Options scripts one fake MX. Zero value: no STARTTLS, plain success.
type Options struct {
	PreCaps       []string // keyword lines Advertised before TLS (e.g. "SIZE 100")
	PostCaps      []string // keyword lines Advertised after TLS
	Advertise     bool     // include STARTTLS in the pre-TLS EHLO reply
	StartTLSReply string   // default "220 Ready to start TLS"
	InjectAfter   string   // bytes glued to the STARTTLS reply in one write
	Handshake     HandshakeMode
	Cert          *tls.Certificate
	PostEHLOReply string // default: 250 multiline; e.g. "502 no"
	FinalReply    string // default "250 2.0.0 queued"
	QuitCloses    bool   // drop the connection instead of answering QUIT
	HeloOnly      bool   // reject EHLO with 502
	DropAfterTLS  bool   // close the connection right after a successful handshake

	// SMTP AUTH scripting. AuthPre/AuthPost are the mechanism lists advertised
	// before/after TLS (e.g. "PLAIN LOGIN"; empty = no AUTH line).
	AuthPre, AuthPost string
	// AuthUser/AuthPass are the credentials the server accepts.
	AuthUser, AuthPass string
	// AuthReply, if set, is sent as the final AUTH reply instead of judging the
	// credentials (e.g. "454 4.7.0 try later", "garbage").
	AuthReply string
	// AuthEcho puts the submitted credentials into the failure reply text, the
	// way a careless server might.
	AuthEcho bool
	// AuthDrop closes the connection on AUTH; AuthStall never answers it;
	// AuthStallMid stalls a LOGIN exchange after the first challenge.
	AuthDrop, AuthStall, AuthStallMid bool
	// RequireAuth answers MAIL with 530 until authentication succeeded.
	RequireAuth bool
	// Capture keeps every received DATA body (dot-unstuffed) for Messages().
	Capture bool
}

// AuthAttempt is one AUTH exchange as decoded by the server.
type AuthAttempt struct {
	Phase, Mechanism, User, Pass string
}

// Server is one scripted fake MX.
type Server struct {
	ln    net.Listener
	opts  Options
	mu    sync.Mutex
	log   []string
	auths []AuthAttempt
	msgs  []string
	Conns atomic.Int32
	wg    sync.WaitGroup
}

// Start listens on 127.0.0.1 and serves until the test ends.
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &Server{ln: ln, opts: o}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			m.Conns.Add(1)
			m.wg.Add(1)
			go func() { defer m.wg.Done(); m.handle(c) }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); m.wg.Wait() })
	return m
}

// Port returns the listening port.
func (m *Server) Port() string { _, p, _ := net.SplitHostPort(m.ln.Addr().String()); return p }

// addr names the server by a host so the client derives its TLS ServerName.
// Addr names the server by host so the client derives its TLS ServerName.
func (m *Server) Addr(host string) string { return net.JoinHostPort(host, m.Port()) }

func (m *Server) record(phase, line string) {
	m.mu.Lock()
	m.log = append(m.log, phase+":"+line)
	m.mu.Unlock()
}

// Commands returns every command received, tagged "plain:" or "tls:".
func (m *Server) Commands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.log...)
}

// sawPlain reports whether any command with the given verb arrived in plaintext.
// SawPlain reports whether a verb arrived over plaintext.
func (m *Server) SawPlain(verb string) bool {
	for _, c := range m.Commands() {
		if strings.HasPrefix(c, "plain:"+verb) {
			return true
		}
	}
	return false
}

// SawAny reports whether a verb arrived in either phase.
func (m *Server) SawAny(verb string) bool {
	for _, c := range m.Commands() {
		if strings.Contains(c, ":"+verb) {
			return true
		}
	}
	return false
}

func (m *Server) handle(raw net.Conn) {
	defer raw.Close()
	c := net.Conn(raw)
	r := bufio.NewReader(c)
	phase := "plain"
	authed := false
	send := func(s string) { _, _ = c.Write([]byte(s)) }
	send("220 mx.test ESMTP\r\n")
	for {
		_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.Fields(line + " x")[0])
		if verb == "AUTH" { // never keep credential payloads, even in test logs
			f := strings.Fields(line)
			m.record(phase, strings.Join(f[:min(2, len(f))], " "))
		} else {
			m.record(phase, line)
		}
		switch verb {
		case "EHLO":
			if m.opts.HeloOnly {
				send("502 5.5.1 EHLO not supported\r\n")
				continue
			}
			caps := m.opts.PreCaps
			if phase == "tls" {
				if m.opts.PostEHLOReply != "" {
					send(m.opts.PostEHLOReply + "\r\n")
					continue
				}
				caps = m.opts.PostCaps
			} else if m.opts.Advertise {
				caps = append([]string{"STARTTLS"}, caps...)
			}
			auth := m.opts.AuthPre
			if phase == "tls" {
				auth = m.opts.AuthPost
			}
			if auth != "" {
				caps = append(append([]string(nil), caps...), "AUTH "+auth)
			}
			lines := append([]string{"mx.test greets you"}, caps...)
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				send("250" + sep + l + "\r\n")
			}
		case "HELO":
			send("250 mx.test\r\n")
		case "STARTTLS":
			reply := m.opts.StartTLSReply
			if reply == "" {
				reply = "220 Ready to start TLS"
			}
			send(reply + "\r\n" + m.opts.InjectAfter)
			if !strings.HasPrefix(reply, "220") {
				continue
			}
			switch m.opts.Handshake {
			case HSClose:
				return
			case HSStall:
				_, _ = io.Copy(io.Discard, c)
				return
			case HSGarbage:
				buf := make([]byte, 16)
				_, _ = c.Read(buf)
				send("this is not TLS\r\n")
				return
			}
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*m.opts.Cert}, MinVersion: tls.VersionTLS12})
			if err := tc.Handshake(); err != nil {
				return
			}
			if m.opts.DropAfterTLS {
				return
			}
			c, r, phase = tc, bufio.NewReader(tc), "tls"
		case "AUTH":
			cont, success := m.handleAuth(phase, line, c, r, send)
			if !cont {
				return
			}
			authed = authed || success
		case "MAIL", "RCPT":
			if verb == "MAIL" && m.opts.RequireAuth && !authed {
				send("530 5.7.0 Authentication required\r\n")
				continue
			}
			send("250 2.1.0 ok\r\n")
		case "DATA":
			send("354 go ahead\r\n")
			var body strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				body.WriteString(strings.TrimPrefix(l, ".")) // undo dot-stuffing (only a leading dot is added)
			}
			if m.opts.Capture {
				m.mu.Lock()
				m.msgs = append(m.msgs, body.String())
				m.mu.Unlock()
			}
			final := m.opts.FinalReply
			if final == "" {
				final = "250 2.0.0 queued"
			}
			send(final + "\r\n")
		case "QUIT":
			if m.opts.QuitCloses {
				return
			}
			send("221 2.0.0 bye\r\n")
			return
		default:
			send("500 5.5.2 unrecognized\r\n")
		}
	}
}

// AuthAttempts returns the AUTH exchanges the server decoded (in memory only).
func (m *Server) AuthAttempts() []AuthAttempt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]AuthAttempt(nil), m.auths...)
}

// handleAuth runs one AUTH exchange. cont=false ends the connection (drop/stall
// scripted, or a read error); success reports a 235.
func (m *Server) handleAuth(phase, line string, c net.Conn, r *bufio.Reader, send func(string)) (cont, success bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		send("501 5.5.4 syntax\r\n")
		return true, false
	}
	mech := strings.ToUpper(f[1])
	if m.opts.AuthDrop {
		return false, false
	}
	if m.opts.AuthStall {
		_, _ = io.Copy(io.Discard, c)
		return false, false
	}
	readLine := func() (string, bool) {
		l, err := r.ReadString('\n')
		return strings.TrimRight(l, "\r\n"), err == nil
	}
	decode := func(s string) string { b, _ := base64.StdEncoding.DecodeString(s); return string(b) }
	var user, pass string
	switch mech {
	case "PLAIN":
		initial := ""
		if len(f) > 2 {
			initial = f[2]
		} else {
			send("334 \r\n")
			var ok bool
			if initial, ok = readLine(); !ok {
				return false, false
			}
		}
		parts := strings.Split(decode(initial), "\x00")
		if len(parts) == 3 {
			user, pass = parts[1], parts[2]
		}
	case "LOGIN":
		send("334 VXNlcm5hbWU6\r\n")
		if m.opts.AuthStallMid {
			_, _ = io.Copy(io.Discard, c)
			return false, false
		}
		u, ok := readLine()
		if !ok {
			return false, false
		}
		send("334 UGFzc3dvcmQ6\r\n")
		p, ok := readLine()
		if !ok {
			return false, false
		}
		user, pass = decode(u), decode(p)
	default:
		send("504 5.5.4 mechanism not supported\r\n")
		return true, false
	}
	m.mu.Lock()
	m.auths = append(m.auths, AuthAttempt{Phase: phase, Mechanism: mech, User: user, Pass: pass})
	m.mu.Unlock()
	switch {
	case m.opts.AuthReply != "":
		send(m.opts.AuthReply + "\r\n")
	case user == m.opts.AuthUser && pass == m.opts.AuthPass:
		send("235 2.7.0 Authentication successful\r\n")
		return true, true
	default:
		text := "535 5.7.8 Authentication credentials invalid"
		if m.opts.AuthEcho {
			text += " for " + user + ":" + pass
		}
		send(text + "\r\n")
	}
	return true, false
}

// Messages returns the DATA bodies received (dot-unstuffed), when Capture is set.
func (m *Server) Messages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.msgs...)
}
