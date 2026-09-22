package routing

import "testing"

func TestSelectMemberDeterministic(t *testing.T) {
	candidates := []string{"m-b", "m-a", "m-c"}
	first := SelectMember("msg-1", candidates)
	for i := 0; i < 20; i++ {
		if got := SelectMember("msg-1", candidates); got != first {
			t.Fatalf("non-deterministic: got %q, want %q", got, first)
		}
	}
}

func TestSelectMemberOrderIndependent(t *testing.T) {
	a := SelectMember("msg-1", []string{"m-a", "m-b", "m-c"})
	b := SelectMember("msg-1", []string{"m-c", "m-a", "m-b"})
	if a != b {
		t.Fatalf("candidate order changed selection: %q vs %q", a, b)
	}
}

func TestSelectMemberEmpty(t *testing.T) {
	if got := SelectMember("msg-1", nil); got != "" {
		t.Fatalf("expected empty result for no candidates, got %q", got)
	}
}

func TestSelectMemberSingle(t *testing.T) {
	if got := SelectMember("msg-1", []string{"only"}); got != "only" {
		t.Fatalf("expected the single candidate, got %q", got)
	}
}

func TestSelectMemberSpreadsAcrossMembers(t *testing.T) {
	candidates := []string{"m-a", "m-b", "m-c", "m-d"}
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		id := SelectMember(hashInput(i), candidates)
		counts[id]++
	}
	if len(counts) != len(candidates) {
		t.Fatalf("expected all %d members to be selected at least once across 400 messages, got %d distinct: %v", len(candidates), len(counts), counts)
	}
}

func hashInput(i int) string {
	const letters = "0123456789abcdef"
	b := make([]byte, 8)
	for j := range b {
		b[j] = letters[(i>>uint(j*4))%len(letters)]
	}
	return string(b)
}
