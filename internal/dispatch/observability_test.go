package dispatch

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/buildinfo"
	"github.com/Ferousco-dev/mailx/internal/database"
	"github.com/Ferousco-dev/mailx/internal/observability"
	"github.com/Ferousco-dev/mailx/internal/queue"
)

type failingQueue struct{ queue.Queue }

func (failingQueue) Enqueue(context.Context, queue.Job) error {
	return errors.New("redis down 10.0.0.9")
}

func TestDispatcherLogsEnqueueFailureWithMessageIDOnly(t *testing.T) {
	ob := newFakeOutbox()
	ob.add(database.OutboxItem{MessageID: "m1", TenantID: "t1", AvailableAt: time.Now()})
	var buf bytes.Buffer
	logger, _ := observability.NewLogger(&buf, "debug", "json")
	m, _ := observability.NewMetrics(buildinfo.Info{Version: "t", Commit: "t"})
	var reported error
	d := New(ob, failingQueue{mustQueue(t)}, WithLogger(logger), WithMetrics(m), WithOnError(func(e error) { reported = e }))
	d.tick(context.Background())
	if reported == nil || ob.dispatched["m1"] {
		t.Fatal("failure must still be reported and the outbox row left pending")
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"dispatch_enqueue_failed"`) || !strings.Contains(out, `"message_id":"m1"`) || strings.Contains(out, "10.0.0.9") {
		t.Fatalf("log = %s", out)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `mailx_queue_operations_total{operation="enqueue",result="error"} 1`) {
		t.Fatal("enqueue error metric missing")
	}
}

func TestDispatcherSuccessLoggedAtDebug(t *testing.T) {
	ob := newFakeOutbox()
	ob.add(database.OutboxItem{MessageID: "m1", TenantID: "t1", AvailableAt: time.Now()})
	var buf bytes.Buffer
	logger, _ := observability.NewLogger(&buf, "debug", "json")
	d := New(ob, mustQueue(t), WithLogger(logger))
	d.tick(context.Background())
	if !strings.Contains(buf.String(), `"msg":"dispatch_enqueued"`) || !ob.dispatched["m1"] {
		t.Fatalf("log = %s", buf.String())
	}
}
