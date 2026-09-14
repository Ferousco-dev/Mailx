package transfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
	"github.com/Ferousco-dev/mailx/internal/smtp"
	"github.com/Ferousco-dev/mailx/internal/storage"
)

func newTestClient(t *testing.T) *smtp.Client {
	t.Helper()
	c, err := smtp.NewClient(smtp.ClientConfig{
		Identity:     "mailx-a.local",
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func startInboundMailX(t *testing.T) (address string, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	server, err := smtp.NewServer(smtp.DefaultConfig(), func(sess smtp.Session, m mail.Message) error {
		rec, err := storage.NewMessageRecord(sess.Envelope, m)
		if err != nil {
			return err
		}
		return store.Save(rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	return ln.Addr().String(), dir
}

func startScriptedSMTP(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); handler(c) }(conn)
		}
	}()
	return ln.Addr().String()
}

// ------------------------ validation --------------------------------------

func TestTransferValidatesRequest(t *testing.T) {
	svc, err := NewService(newTestClient(t))
	if err != nil {
		t.Fatal(err)
	}
	// empty destination
	res, err := svc.Transfer(context.Background(), Request{Envelope: mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}}})
	if err == nil || res.Accepted || res.FailureStage != smtp.StageInvalidInput {
		t.Fatalf("empty destination: %v %+v", err, res)
	}
	// zero recipients
	res, err = svc.Transfer(context.Background(), Request{Destination: "127.0.0.1:1", Envelope: mail.Envelope{MailFrom: "<a@b>"}})
	if err == nil || res.Accepted || res.FailureStage != smtp.StageInvalidInput {
		t.Fatalf("zero recipients: %v %+v", err, res)
	}
}

func TestTransferNilClientRejected(t *testing.T) {
	if _, err := NewService(nil); err == nil {
		t.Fatal("expected error for nil client")
	}
}

// ------------------------ MailX A → MailX B ------------------------------

func TestTransferMailXAToMailXB(t *testing.T) {
	addr, dir := startInboundMailX(t)
	svc, _ := NewService(newTestClient(t))

	raw := "From: Alice <alice@example.com>\r\n" +
		"To: Bob <bob@example.com>\r\n" +
		"Subject: MailX A to B\r\n" +
		"Message-ID: <ab-1@mailx.test>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"B\"\r\n" +
		"\r\n" +
		"--B\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"hello\r\n" +
		".leading dot\r\n" +
		"--B\r\n" +
		"Content-Type: text/html\r\n\r\n" +
		"<b>hi</b>\r\n" +
		"--B\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"Content-Disposition: attachment; filename=\"n.txt\"\r\n\r\n" +
		"aGVsbG8gYXR0YWNobWVudA==\r\n" +
		"--B--\r\n"

	res, err := svc.Transfer(context.Background(), Request{
		Destination: addr,
		Envelope: mail.Envelope{
			MailFrom:   "<bounce@mailx-a.local>",
			Recipients: []string{"<bob@mailx-b.local>", "<hidden-bcc@mailx-b.local>"},
		},
		Raw: raw,
	})
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	if !res.Accepted || res.FinalCode != 250 {
		t.Fatalf("not accepted: %+v", res)
	}
	if res.AttemptID == "" {
		t.Fatal("attempt id missing")
	}
	if res.StartedAt.After(res.FinishedAt) {
		t.Fatalf("timestamps inverted: %+v", res)
	}
	if res.Duration() < 0 {
		t.Fatalf("negative duration: %v", res.Duration())
	}

	// Verify remote persistence
	deadline := time.Now().Add(2 * time.Second)
	var ids []string
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(filepath.Join(dir, "messages"))
		if len(entries) == 1 {
			for _, e := range entries {
				ids = append(ids, e.Name())
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ids) != 1 {
		t.Fatalf("receiver did not persist exactly one message: %v", ids)
	}
	store, _ := storage.NewFileStore(dir)
	loaded, err := store.Load(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Metadata.Envelope.MailFrom != "<bounce@mailx-a.local>" {
		t.Fatalf("envelope MAIL FROM: %q", loaded.Metadata.Envelope.MailFrom)
	}
	if len(loaded.Metadata.Envelope.RcptTo) != 2 || loaded.Metadata.Envelope.RcptTo[1] != "<hidden-bcc@mailx-b.local>" {
		t.Fatalf("envelope recipients not preserved: %v", loaded.Metadata.Envelope.RcptTo)
	}
	if loaded.Metadata.Message.From != "Alice <alice@example.com>" {
		t.Fatalf("From header lost: %q", loaded.Metadata.Message.From)
	}
	if len(loaded.Metadata.Attachments) != 1 || loaded.Metadata.Attachments[0].Filename != "n.txt" {
		t.Fatalf("attachment metadata lost: %+v", loaded.Metadata.Attachments)
	}
	eml, err := os.ReadFile(filepath.Join(dir, "messages", ids[0], "message.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(eml), "\r\n.leading dot\r\n") {
		t.Fatalf("dot-transparency broken; eml=%q", string(eml))
	}
	att, err := os.ReadFile(filepath.Join(dir, "messages", ids[0], "attachments", "0001.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(att) != "hello attachment" {
		t.Fatalf("attachment content: %q", string(att))
	}
}

// ------------------------ failure paths -----------------------------------

func scriptRejectAt(_ *testing.T, phase, reply string) func(net.Conn) {
	return func(c net.Conn) {
		r := bufio.NewReader(c)
		_, _ = c.Write([]byte("220 ok\r\n"))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO"):
				if phase == "EHLO" {
					_, _ = c.Write([]byte(reply))
					return
				}
				_, _ = c.Write([]byte("250 ok\r\n"))
			case strings.HasPrefix(upper, "MAIL"):
				if phase == "MAIL" {
					_, _ = c.Write([]byte(reply))
					return
				}
				_, _ = c.Write([]byte("250 ok\r\n"))
			case strings.HasPrefix(upper, "RCPT"):
				if phase == "RCPT" {
					_, _ = c.Write([]byte(reply))
					return
				}
				_, _ = c.Write([]byte("250 ok\r\n"))
			case strings.HasPrefix(upper, "DATA"):
				if phase == "DATA" {
					_, _ = c.Write([]byte(reply))
					return
				}
				_, _ = c.Write([]byte("354 go\r\n"))
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if strings.TrimRight(l, "\r\n") == "." {
						break
					}
				}
				if phase == "FINAL" {
					_, _ = c.Write([]byte(reply))
					return
				}
				_, _ = c.Write([]byte("250 accepted\r\n"))
			case strings.HasPrefix(upper, "QUIT"):
				if phase == "QUIT" {
					// disconnect
					return
				}
				_, _ = c.Write([]byte("221 bye\r\n"))
				return
			}
		}
	}
}

func TestTransferDestinationDown(t *testing.T) {
	svc, _ := NewService(newTestClient(t))
	res, err := svc.Transfer(context.Background(), Request{
		Destination: "127.0.0.1:1",
		Envelope:    mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}},
		Raw:         "x\r\n",
	})
	if err == nil || res.Accepted {
		t.Fatalf("expected failure, got %v %+v", err, res)
	}
	if res.FailureStage != smtp.StageDial {
		t.Fatalf("expected dial stage, got %q", res.FailureStage)
	}
	if res.FinalCode != 0 {
		t.Fatalf("network failure must not carry SMTP code: %d", res.FinalCode)
	}
}

func TestTransferTemporarySMTPFailure(t *testing.T) {
	addr := startScriptedSMTP(t, scriptRejectAt(t, "MAIL", "451 4.7.0 back off\r\n"))
	svc, _ := NewService(newTestClient(t))
	res, err := svc.Transfer(context.Background(), Request{
		Destination: addr,
		Envelope:    mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}},
		Raw:         "x\r\n",
	})
	if err == nil || res.Accepted {
		t.Fatalf("expected failure, got %v %+v", err, res)
	}
	if !res.Temporary || !IsTemporary(err) {
		t.Fatalf("must be temporary: %+v %v", res, err)
	}
	if res.FinalCode != 451 || res.EnhancedStatus != "4.7.0" || res.FailureStage != smtp.StageMailFrom {
		t.Fatalf("metadata not preserved: %+v", res)
	}
}

func TestTransferPermanentSMTPFailure(t *testing.T) {
	addr := startScriptedSMTP(t, scriptRejectAt(t, "MAIL", "550 5.7.1 blocked\r\n"))
	svc, _ := NewService(newTestClient(t))
	res, err := svc.Transfer(context.Background(), Request{
		Destination: addr,
		Envelope:    mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}},
		Raw:         "x\r\n",
	})
	if err == nil || res.Accepted || res.Temporary || IsTemporary(err) {
		t.Fatalf("expected permanent failure, got %+v %v", res, err)
	}
	if res.FinalCode != 550 || res.EnhancedStatus != "5.7.1" {
		t.Fatalf("metadata: %+v", res)
	}
}

func TestTransferRecipientFailureMetadata(t *testing.T) {
	addr := startScriptedSMTP(t, func(c net.Conn) {
		r := bufio.NewReader(c)
		_, _ = c.Write([]byte("220 ok\r\n"))
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			u := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(u, "EHLO"), strings.HasPrefix(u, "MAIL"):
				_, _ = c.Write([]byte("250 ok\r\n"))
			case strings.HasPrefix(u, "RCPT TO:<BAD"):
				_, _ = c.Write([]byte("550 5.1.1 user unknown\r\n"))
				return
			case strings.HasPrefix(u, "RCPT"):
				_, _ = c.Write([]byte("250 ok\r\n"))
			}
		}
	})
	svc, _ := NewService(newTestClient(t))
	res, err := svc.Transfer(context.Background(), Request{
		Destination: addr,
		Envelope:    mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<ok@x>", "<bad@x>"}},
		Raw:         "x\r\n",
	})
	if err == nil || res.Accepted {
		t.Fatal("expected failure")
	}
	if res.FailureStage != smtp.StageRcptTo || res.Recipient != "<bad@x>" || res.FinalCode != 550 {
		t.Fatalf("recipient metadata: %+v", res)
	}
	var te *TransferError
	if !errors.As(err, &te) || te.Recipient != "<bad@x>" {
		t.Fatalf("recipient not on error: %v", err)
	}
}

func TestTransferFinalDATAFailurePreventsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reply    string
		wantTemp bool
	}{
		{"final 451", "451 4.3.0 later\r\n", true},
		{"final 550", "550 5.6.0 rejected\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := startScriptedSMTP(t, scriptRejectAt(t, "FINAL", tc.reply))
			svc, _ := NewService(newTestClient(t))
			res, err := svc.Transfer(context.Background(), Request{
				Destination: addr,
				Envelope:    mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}},
				Raw:         "x\r\n",
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if res.Accepted {
				t.Fatalf("final-DATA failure must not mark accepted: %+v", res)
			}
			if res.FailureStage != smtp.StageDataResponse {
				t.Fatalf("stage: %q", res.FailureStage)
			}
			if res.Temporary != tc.wantTemp {
				t.Fatalf("temporary=%v want %v", res.Temporary, tc.wantTemp)
			}
		})
	}
}

func TestTransferAcceptedPreservesQuitFailure(t *testing.T) {
	addr := startScriptedSMTP(t, scriptRejectAt(t, "QUIT", ""))
	svc, _ := NewService(newTestClient(t))
	res, err := svc.Transfer(context.Background(), Request{
		Destination: addr,
		Envelope:    mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}},
		Raw:         "x\r\n",
	})
	if err != nil {
		t.Fatalf("accepted transfer must not return error, got %v", err)
	}
	if !res.Accepted {
		t.Fatalf("expected Accepted, got %+v", res)
	}
	if res.QuitError == "" {
		t.Fatalf("expected QuitError to be recorded: %+v", res)
	}
}

func TestTransferContextCancellation(t *testing.T) {
	addr := startScriptedSMTP(t, func(c net.Conn) {
		// hang forever without greeting
		buf := make([]byte, 1)
		_, _ = c.Read(buf)
	})
	svc, _ := NewService(newTestClient(t))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	res, err := svc.Transfer(ctx, Request{
		Destination: addr,
		Envelope:    mail.Envelope{MailFrom: "<a@b>", Recipients: []string{"<c@d>"}},
		Raw:         "x\r\n",
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if res.Accepted {
		t.Fatal("must not accept on cancellation")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("cancellation slow: %v", time.Since(start))
	}
}

func TestTransferConcurrent(t *testing.T) {
	addr, _ := startInboundMailX(t)
	svc, _ := NewService(newTestClient(t))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, err := svc.Transfer(context.Background(), Request{
				Destination: addr,
				Envelope:    mail.Envelope{MailFrom: "<a@b>", Recipients: []string{fmt.Sprintf("<r%d@x>", n)}},
				Raw:         fmt.Sprintf("Subject: c%d\r\n\r\nbody %d\r\n", n, n),
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent: %v", err)
		}
	}
}

func TestTransferAttemptIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newAttemptID()
		if seen[id] {
			t.Fatalf("collision at %d", i)
		}
		seen[id] = true
	}
}
