package smtp

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

type recordingObserver struct {
	mu     sync.Mutex
	events []string
}

func (o *recordingObserver) add(s string)             { o.mu.Lock(); o.events = append(o.events, s); o.mu.Unlock() }
func (o *recordingObserver) SessionStarted(id string) { o.add("start:" + id) }
func (o *recordingObserver) SessionEnded(id string, _ time.Duration, failed bool) {
	if failed {
		o.add("end-failed:" + id)
	} else {
		o.add("end:" + id)
	}
}
func (o *recordingObserver) SessionRejected() { o.add("rejected") }
func (o *recordingObserver) MessageResult(id, result, reason string) {
	o.add("msg:" + id + ":" + result + ":" + reason)
}

func (o *recordingObserver) all() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return strings.Join(o.events, "|")
}

type panickingObserver struct{ recordingObserver }

func (*panickingObserver) SessionStarted(string)                    { panic("observer bug") }
func (*panickingObserver) MessageResult(_, _, _ string)             { panic("observer bug") }
func (*panickingObserver) SessionEnded(string, time.Duration, bool) { panic("observer bug") }

func runSession(t *testing.T, cfg Config, sink func(Session, mail.Message) error) {
	t.Helper()
	srv, cli := net.Pipe()
	defer cli.Close()
	done := make(chan struct{})
	go func() { _ = HandleConnectionWithConfig(srv, cfg, sink); close(done) }()
	r := bufio.NewReader(cli)
	expect(t, r, "220")
	for _, c := range []string{"EHLO localhost", "MAIL FROM:<PRIVATE-SENDER@example.com>", "RCPT TO:<PRIVATE-RCPT@example.com>", "DATA"} {
		_, _ = cli.Write([]byte(c + "\r\n"))
		if c == "DATA" {
			expect(t, r, "354")
		} else {
			expect(t, r, "250")
		}
	}
	_, _ = cli.Write([]byte("Subject: PRIVATE-SUBJECT\r\n\r\nPRIVATE-BODY\r\n.\r\n"))
	_, _ = r.ReadString('\n') // final reply (250 or 451)
	_, _ = cli.Write([]byte("QUIT\r\n"))
	expect(t, r, "221")
	<-done
}

func TestObserverSeesAcceptedMessageWithSessionIDAndNoContent(t *testing.T) {
	obs := &recordingObserver{}
	cfg := DefaultConfig()
	cfg.Observer = obs
	var sessionID string
	runSession(t, cfg, func(s Session, _ mail.Message) error { sessionID = s.ID; return nil })
	got := obs.all()
	if !strings.HasPrefix(sessionID, "sess_") || !strings.Contains(got, "start:"+sessionID) ||
		!strings.Contains(got, "msg:"+sessionID+":accepted:") || !strings.Contains(got, "end:"+sessionID) {
		t.Fatalf("session %q events: %s", sessionID, got)
	}
	for _, marker := range []string{"PRIVATE-SENDER", "PRIVATE-RCPT", "PRIVATE-SUBJECT", "PRIVATE-BODY"} {
		if strings.Contains(got, marker) {
			t.Fatalf("observer events leaked %s: %s", marker, got)
		}
	}
}

func TestObserverSeesTemporarySinkFailure(t *testing.T) {
	obs := &recordingObserver{}
	cfg := DefaultConfig()
	cfg.Observer = obs
	runSession(t, cfg, func(Session, mail.Message) error { return errors.New("disk full at /secret/path") })
	got := obs.all()
	if !strings.Contains(got, ":temporary_failure:sink_failed") || strings.Contains(got, "secret/path") {
		t.Fatalf("events: %s", got)
	}
}

func TestObserverPanicNeverAffectsSession(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Observer = &panickingObserver{}
	stored := false
	runSession(t, cfg, func(Session, mail.Message) error { stored = true; return nil })
	if !stored {
		t.Fatal("observer panic prevented message storage")
	}
}

func TestSessionIDsAreUniquePerConnection(t *testing.T) {
	a, b := newSessionID(), newSessionID()
	if a == b || len(a) < 10 {
		t.Fatalf("ids %q %q", a, b)
	}
}
