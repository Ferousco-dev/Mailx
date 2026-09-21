package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLoggerValidatesConfig(t *testing.T) {
	for _, lvl := range []string{"", "debug", "INFO", "warn", "error"} {
		if _, err := NewLogger(&bytes.Buffer{}, lvl, "json"); err != nil {
			t.Fatalf("level %q rejected: %v", lvl, err)
		}
	}
	for _, f := range []string{"", "json", "TEXT"} {
		if _, err := NewLogger(&bytes.Buffer{}, "info", f); err != nil {
			t.Fatalf("format %q rejected: %v", f, err)
		}
	}
	if _, err := NewLogger(&bytes.Buffer{}, "verbose", "json"); err == nil {
		t.Fatal("invalid level accepted")
	}
	if _, err := NewLogger(&bytes.Buffer{}, "info", "xml"); err == nil {
		t.Fatal("invalid format accepted")
	}
}

func TestLoggerJSONAndTextOutput(t *testing.T) {
	var buf bytes.Buffer
	l, _ := NewLogger(&buf, "info", "json")
	l.Info("evt", "request_id", "req_1")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v: %s", err, buf.String())
	}
	if rec["msg"] != "evt" || rec["request_id"] != "req_1" || rec["level"] != "INFO" {
		t.Fatalf("record = %v", rec)
	}
	buf.Reset()
	l, _ = NewLogger(&buf, "info", "text")
	l.Info("evt", "request_id", "req_1")
	if out := buf.String(); !strings.Contains(out, "msg=evt") || !strings.Contains(out, "request_id=req_1") {
		t.Fatalf("text output = %q", out)
	}
}

func TestLoggerLevelFiltering(t *testing.T) {
	cases := map[string][]string{
		"debug": {"d", "i", "w", "e"}, "info": {"i", "w", "e"}, "warn": {"w", "e"}, "error": {"e"},
	}
	for level, want := range cases {
		var buf bytes.Buffer
		l, _ := NewLogger(&buf, level, "text")
		l.Debug("d")
		l.Info("i")
		l.Warn("w")
		l.Error("e")
		for _, m := range []string{"d", "i", "w", "e"} {
			has := strings.Contains(buf.String(), "msg="+m+"\n") || strings.HasSuffix(buf.String(), "msg="+m)
			expected := false
			for _, w := range want {
				expected = expected || w == m
			}
			if has != expected {
				t.Fatalf("level %s message %s: present=%v want=%v\n%s", level, m, has, expected, buf.String())
			}
		}
	}
}

type panicHandler struct{ slog.Handler }

func (panicHandler) Handle(context.Context, slog.Record) error { panic("sink exploded") }

func TestLoggerSinkPanicIsSwallowed(t *testing.T) {
	l := slog.New(safeHandler{panicHandler{slog.NewTextHandler(&bytes.Buffer{}, nil)}})
	l.Info("must not panic")
	l.With("k", "v").WithGroup("g").Info("still fine")
}

func TestDiscardLogger(t *testing.T) {
	Discard().Error("nothing happens")
}
