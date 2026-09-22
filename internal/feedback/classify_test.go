package feedback

import "testing"

func TestClassifyPermanentFailure(t *testing.T) {
	k, s := Classify(ParsedDSN{Action: ActionFailed, Status: "5.1.1"})
	if k != KindBouncePermanent || s.String() != "5.1.1" {
		t.Fatalf("%v %v", k, s)
	}
}

func TestClassifyDelayedNeverPermanent(t *testing.T) {
	k, _ := Classify(ParsedDSN{Action: ActionDelayed, Status: "4.4.7"})
	if k != KindBounceTemporary {
		t.Fatalf("%v", k)
	}
}

func TestClassifyInconsistentActionStatusIsUnknown(t *testing.T) {
	// Action says failed but the status class is temporary: internally
	// inconsistent, must not be guessed into either bucket.
	k, _ := Classify(ParsedDSN{Action: ActionFailed, Status: "4.2.2"})
	if k != KindBounceUnknown {
		t.Fatalf("%v", k)
	}
	k, _ = Classify(ParsedDSN{Action: ActionDelayed, Status: "5.1.1"})
	if k != KindBounceUnknown {
		t.Fatalf("%v", k)
	}
}

func TestClassifyDeliveredRelayedExpandedAreUnknown(t *testing.T) {
	for _, a := range []Action{ActionDelivered, ActionRelayed, ActionExpanded, ActionUnknown} {
		k, _ := Classify(ParsedDSN{Action: a, Status: "2.1.5"})
		if k != KindBounceUnknown {
			t.Fatalf("action %v -> %v", a, k)
		}
	}
}

func TestClassifyMissingOrMalformedStatusIsUnknown(t *testing.T) {
	for _, status := range []string{"", "garbage", "5.1", "9.1.1", "5..1"} {
		k, _ := Classify(ParsedDSN{Action: ActionFailed, Status: status})
		if k != KindBounceUnknown {
			t.Fatalf("status %q -> %v, want unknown", status, k)
		}
	}
}
