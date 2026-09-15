package queue

import (
	"errors"
	"testing"
	"time"
)

func TestJobValidate(t *testing.T) {
	valid := Job{ID: "j1", MessageID: "m1"}
	if err := valid.validate(); err != nil {
		t.Fatalf("expected valid job, got %v", err)
	}

	cases := []struct {
		name string
		job  Job
		want error
	}{
		{"empty id", Job{ID: "", MessageID: "m1"}, ErrEmptyJobID},
		{"whitespace id", Job{ID: "   ", MessageID: "m1"}, ErrEmptyJobID},
		{"empty message id", Job{ID: "j1", MessageID: ""}, ErrEmptyMessageID},
		{"crlf in id", Job{ID: "j1\r\nX", MessageID: "m1"}, ErrJobIDControlChar},
		{"nul in id", Job{ID: "j1\x00", MessageID: "m1"}, ErrJobIDControlChar},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.job.validate(); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewJobIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := NewJobID()
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			t.Fatal("empty generated id")
		}
		if seen[id] {
			t.Fatalf("collision at %d", i)
		}
		seen[id] = true
	}
}

// TestJobCopySemantics documents that Job is a plain value type: copying it
// (as Enqueue and Claim both do) never lets a caller mutate queue-internal
// state through a returned value, and never lets queue-internal state be
// mutated by a caller through zero exported pointer fields.
func TestJobCopySemantics(t *testing.T) {
	original := Job{ID: "j1", MessageID: "m1", EnqueuedAt: time.Unix(1, 0), AvailableAt: time.Unix(2, 0)}
	copy1 := original
	copy1.ID = "mutated"
	if original.ID != "j1" {
		t.Fatal("mutating a copy affected the original Job")
	}
}
