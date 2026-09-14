package smtp

import (
	"bufio"
	"errors"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

func TestServerConnectionLimitConfiguration(t *testing.T) {
	defaults, err := NewServer(DefaultConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cap(defaults.slots) != DefaultMaxConnections {
		t.Fatalf("default slot capacity=%d, want %d", cap(defaults.slots), DefaultMaxConnections)
	}

	custom, err := NewServer(limitedConfig(7), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cap(custom.slots) != 7 {
		t.Fatalf("custom slot capacity=%d, want 7", cap(custom.slots))
	}

	invalid := limitedConfig(-1)
	if _, err := NewServer(invalid, nil); err == nil {
		t.Fatal("negative MaxConnections was accepted")
	}
}

func TestConnectionLimitExactAndSlotReuse(t *testing.T) {
	server, listener := startLimitedSMTPServer(t, limitedConfig(2), nil)
	a, readerA := connectLimitedClient(t, listener)
	b, readerB := connectLimitedClient(t, listener)
	expectSMTPBanner(t, a, readerA)
	expectSMTPBanner(t, b, readerB)
	waitForActiveConnections(t, server, 2)

	c, _ := connectLimitedClient(t, listener)
	expectClosedWithoutReply(t, c)
	waitForActiveConnections(t, server, 2)

	writeSMTP(t, a, "QUIT\r\n")
	expectRawCRLF(t, readerA, "221")
	waitForActiveConnections(t, server, 1)

	d, readerD := connectLimitedClient(t, listener)
	expectSMTPBanner(t, d, readerD)
	waitForActiveConnections(t, server, 2)

	// EOF and QUIT are separate handler return paths; both must release once.
	b.Close()
	waitForActiveConnections(t, server, 1)
	writeSMTP(t, d, "QUIT\r\n")
	expectRawCRLF(t, readerD, "221")
	waitForActiveConnections(t, server, 0)
}

func TestConnectionSlotReleasedAfterReadTimeout(t *testing.T) {
	config := limitedConfig(1)
	config.ReadTimeout = 50 * time.Millisecond
	server, listener := startLimitedSMTPServer(t, config, nil)
	idle, idleReader := connectLimitedClient(t, listener)
	expectSMTPBanner(t, idle, idleReader)
	waitForActiveConnections(t, server, 1)
	waitForActiveConnections(t, server, 0)
	expectClosedWithoutReply(t, idle)

	replacement, replacementReader := connectLimitedClient(t, listener)
	expectSMTPBanner(t, replacement, replacementReader)
	writeSMTP(t, replacement, "QUIT\r\n")
	expectRawCRLF(t, replacementReader, "221")
	waitForActiveConnections(t, server, 0)
}

func TestConnectionLimitsAreIndependentPerServer(t *testing.T) {
	serverA, listenerA := startLimitedSMTPServer(t, limitedConfig(1), nil)
	serverB, listenerB := startLimitedSMTPServer(t, limitedConfig(3), nil)

	a1, readerA1 := connectLimitedClient(t, listenerA)
	expectSMTPBanner(t, a1, readerA1)
	b1, readerB1 := connectLimitedClient(t, listenerB)
	b2, readerB2 := connectLimitedClient(t, listenerB)
	b3, readerB3 := connectLimitedClient(t, listenerB)
	expectSMTPBanner(t, b1, readerB1)
	expectSMTPBanner(t, b2, readerB2)
	expectSMTPBanner(t, b3, readerB3)
	waitForActiveConnections(t, serverA, 1)
	waitForActiveConnections(t, serverB, 3)

	a2, _ := connectLimitedClient(t, listenerA)
	b4, _ := connectLimitedClient(t, listenerB)
	expectClosedWithoutReply(t, a2)
	expectClosedWithoutReply(t, b4)

	a1.Close()
	b1.Close()
	b2.Close()
	b3.Close()
	waitForActiveConnections(t, serverA, 0)
	waitForActiveConnections(t, serverB, 0)
}

func TestConnectionStormNeverExceedsLimit(t *testing.T) {
	const (
		limit    = 5
		attempts = 40
	)
	server, listener := startLimitedSMTPServer(t, limitedConfig(limit), nil)
	clients := make([]net.Conn, attempts)
	serverConnections := make([]net.Conn, attempts)
	for index := range attempts {
		serverConnections[index], clients[index] = net.Pipe()
		t.Cleanup(func() { clients[index].Close() })
	}

	var submissions sync.WaitGroup
	submissions.Add(attempts)
	for _, conn := range serverConnections {
		go func() {
			defer submissions.Done()
			if err := listener.enqueue(conn); err != nil {
				conn.Close()
			}
		}()
	}
	submissions.Wait()

	admitted := 0
	for _, client := range clients {
		_ = client.SetReadDeadline(time.Now().Add(time.Second))
		line, err := bufio.NewReader(client).ReadString('\n')
		if err == nil && line == "220 localhost MailX SMTP Server\r\n" {
			admitted++
			continue
		}
		if line != "" || err == nil {
			t.Fatalf("unexpected overload result: line=%q err=%v", line, err)
		}
	}
	if admitted != limit {
		t.Fatalf("admitted=%d, want exactly %d", admitted, limit)
	}
	waitForActiveConnections(t, server, limit)

	for _, client := range clients {
		client.Close()
	}
	waitForActiveConnections(t, server, 0)
	recovered, recoveredReader := connectLimitedClient(t, listener)
	expectSMTPBanner(t, recovered, recoveredReader)
	writeSMTP(t, recovered, "QUIT\r\n")
	expectRawCRLF(t, recoveredReader, "221")
	waitForActiveConnections(t, server, 0)
}

func TestLimitedServerPreservesNormalSMTP(t *testing.T) {
	var received mail.Message
	server, listener := startLimitedSMTPServer(t, limitedConfig(1), func(_ Session, message mail.Message) error {
		received = message
		return nil
	})
	client, reader := connectLimitedClient(t, listener)
	expectSMTPBanner(t, client, reader)
	beginDataCommandsForSizeTest(t, client, reader, "sender@example.com", "recipient@example.com")
	wire := "Subject: normal\r\n\r\n..dot\r\n"
	writeSMTP(t, client, wire+".\r\n")
	expectRawCRLF(t, reader, "250")
	writeSMTP(t, client, "QUIT\r\n")
	expectRawCRLF(t, reader, "221")
	waitForActiveConnections(t, server, 0)
	if received.Raw != "Subject: normal\r\n\r\n.dot\r\n" {
		t.Fatalf("normal SMTP changed: %#v", received)
	}
}

func limitedConfig(maxConnections int) Config {
	return Config{
		CommandLineLimit: 512,
		ReadTimeout:      time.Second,
		WriteTimeout:     time.Second,
		MaxMessageSize:   1024 * 1024,
		MaxConnections:   maxConnections,
	}
}

func startLimitedSMTPServer(t *testing.T, config Config, sink func(Session, mail.Message) error) (*Server, *testListener) {
	t.Helper()
	server, err := NewServer(config, sink)
	if err != nil {
		t.Fatal(err)
	}
	listener := newTestListener()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned an error: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("Serve did not stop after listener close")
		}
	})
	return server, listener
}

func connectLimitedClient(t *testing.T, listener *testListener) (net.Conn, *bufio.Reader) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close() })
	if err := listener.enqueue(server); err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, bufio.NewReader(client)
}

func expectSMTPBanner(t *testing.T, conn net.Conn, reader *bufio.Reader) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	expectRawCRLF(t, reader, "220")
	_ = conn.SetReadDeadline(time.Time{})
}

func expectClosedWithoutReply(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buffer := make([]byte, 1)
	count, err := conn.Read(buffer)
	if count != 0 || err == nil {
		t.Fatalf("over-limit connection returned count=%d data=%q err=%v", count, buffer[:count], err)
	}
}

func waitForActiveConnections(t *testing.T, server *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(server.slots) != want {
		if time.Now().After(deadline) {
			t.Fatalf("active connections=%d, want %d", len(server.slots), want)
		}
		runtime.Gosched()
	}
}

type testListener struct {
	connections chan net.Conn
	closed      chan struct{}
	closeOnce   sync.Once
}

func newTestListener() *testListener {
	return &testListener{connections: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *testListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *testListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *testListener) Addr() net.Addr {
	return testAddr("mailx-test")
}

func (l *testListener) enqueue(conn net.Conn) error {
	select {
	case l.connections <- conn:
		return nil
	case <-l.closed:
		return net.ErrClosed
	}
}

type testAddr string

func (a testAddr) Network() string { return string(a) }
func (a testAddr) String() string  { return string(a) }

func TestServerReturnsAcceptErrors(t *testing.T) {
	want := errors.New("accept failed")
	server, err := NewServer(limitedConfig(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	err = server.Serve(errorListener{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("Serve error=%v, want wrapped %v", err, want)
	}
}

type errorListener struct{ err error }

func (l errorListener) Accept() (net.Conn, error) { return nil, l.err }
func (l errorListener) Close() error              { return nil }
func (l errorListener) Addr() net.Addr            { return testAddr("error") }
