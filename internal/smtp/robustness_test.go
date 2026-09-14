package smtp

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

func TestConfigValidation(t *testing.T) {
	defaults := DefaultConfig()
	if defaults.CommandLineLimit != 512 || defaults.ReadTimeout != 5*time.Minute || defaults.WriteTimeout != 5*time.Minute || defaults.MaxMessageSize != 10*1024*1024 {
		t.Fatalf("unexpected defaults: %#v", defaults)
	}
	valid := []Config{
		defaults,
		{CommandLineLimit: 512, ReadTimeout: 0, WriteTimeout: time.Second},
		{CommandLineLimit: 512, ReadTimeout: time.Second, WriteTimeout: 0},
		{CommandLineLimit: 1024, ReadTimeout: time.Millisecond, WriteTimeout: time.Millisecond},
		{CommandLineLimit: 512, MaxMessageSize: 1},
	}
	for _, config := range valid {
		if err := config.validate(); err != nil {
			t.Fatalf("valid config rejected: %#v: %v", config, err)
		}
	}
	invalid := []Config{
		{CommandLineLimit: 0},
		{CommandLineLimit: -1},
		{CommandLineLimit: 512, ReadTimeout: -time.Second},
		{CommandLineLimit: 512, WriteTimeout: -time.Second},
		{CommandLineLimit: 512, MaxMessageSize: -1},
	}
	for _, config := range invalid {
		if err := config.validate(); err == nil {
			t.Fatalf("invalid config accepted: %#v", config)
		}
	}

	normalized, err := (Config{CommandLineLimit: 512}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	if normalized.MaxMessageSize != DefaultMaxMessageSize {
		t.Fatalf("zero maximum did not select the finite default: %#v", normalized)
	}
}

func TestCommandLineLimitIncludesCRLF(t *testing.T) {
	const limit = 512
	t.Run("exact boundary", func(t *testing.T) {
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- HandleConnectionWithConfig(server, Config{CommandLineLimit: limit}, nil) }()
		reader := bufio.NewReader(client)
		expectRawCRLF(t, reader, "220")
		line := "NOOP" + strings.Repeat(" ", limit-2-len("NOOP")) + "\r\n"
		_, _ = client.Write([]byte(line))
		expectRawCRLF(t, reader, "250")
		_, _ = client.Write([]byte("QUIT\r\n"))
		expectRawCRLF(t, reader, "221")
		client.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	for _, test := range []struct {
		name string
		line string
	}{
		{"one byte over", "NOOP" + strings.Repeat(" ", limit-1-len("NOOP")) + "\r\n"},
		{"large without newline", strings.Repeat("X", 1<<20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, client := net.Pipe()
			done := make(chan error, 1)
			go func() {
				done <- HandleConnectionWithConfig(server, Config{CommandLineLimit: limit, WriteTimeout: time.Second}, nil)
			}()
			reader := bufio.NewReader(client)
			expectRawCRLF(t, reader, "220")
			writeDone := make(chan struct{})
			go func() { _, _ = client.Write([]byte(test.line)); close(writeDone) }()
			expectRawCRLF(t, reader, "500")
			client.Close()
			select {
			case <-writeDone:
			case <-time.After(time.Second):
				t.Fatal("oversized write did not unblock")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("server did not close oversized command")
			}
		})
	}
}

func TestReadTimeoutsAndRefresh(t *testing.T) {
	config := Config{CommandLineLimit: 512, ReadTimeout: 100 * time.Millisecond, WriteTimeout: time.Second}
	t.Run("idle command", func(t *testing.T) {
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- HandleConnectionWithConfig(server, config, nil) }()
		expectRawCRLF(t, bufio.NewReader(client), "220")
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("idle connection did not time out")
		}
		client.Close()
	})

	t.Run("refresh between commands", func(t *testing.T) {
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- HandleConnectionWithConfig(server, config, nil) }()
		reader := bufio.NewReader(client)
		expectRawCRLF(t, reader, "220")
		for range 3 {
			time.Sleep(60 * time.Millisecond)
			_, _ = client.Write([]byte("NOOP\r\n"))
			expectRawCRLF(t, reader, "250")
		}
		_, _ = client.Write([]byte("QUIT\r\n"))
		expectRawCRLF(t, reader, "221")
		client.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("during DATA", func(t *testing.T) {
		server, client := net.Pipe()
		done := make(chan error, 1)
		go func() { done <- HandleConnectionWithConfig(server, config, nil) }()
		reader := bufio.NewReader(client)
		expectRawCRLF(t, reader, "220")
		for _, command := range []string{"EHLO x", "MAIL FROM:<a@x>", "RCPT TO:<b@x>", "DATA"} {
			_, _ = client.Write([]byte(command + "\r\n"))
			expectRawCRLF(t, reader, map[bool]string{true: "354", false: "250"}[command == "DATA"])
			if command == "EHLO x" {
				expectRawCRLF(t, reader, "250")
			}
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("DATA read did not time out")
		}
		client.Close()
	})
}

func TestZeroTimeoutsDisableDeadlines(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- HandleConnectionWithConfig(server, Config{CommandLineLimit: 512}, nil) }()
	reader := bufio.NewReader(client)
	expectRawCRLF(t, reader, "220")
	time.Sleep(120 * time.Millisecond)
	_, _ = client.Write([]byte("QUIT\r\n"))
	expectRawCRLF(t, reader, "221")
	client.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWriteTimeoutClosesBlockedConnection(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- HandleConnectionWithConfig(server, Config{CommandLineLimit: 512, WriteTimeout: 50 * time.Millisecond}, nil)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked greeting write returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked write did not time out")
	}
	client.Close()
}

func expectRawCRLF(t *testing.T, reader *bufio.Reader, prefix string) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "\r\n") {
		t.Fatalf("reply=%q err=%v", line, err)
	}
}
