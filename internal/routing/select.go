// Package routing implements v0.39 sending-pool member selection: which
// pool member a message durably routes through, and the process-local
// transport registry that dials for each member.
//
// Three layers stay distinct here on purpose (see .ilana/architecture.md
// "v0.39 Sending Pools"): configured policy (pools/members, mutable), the
// durable per-message routing decision (made once, read-only afterwards),
// and the actual per-attempt transport identity (a historical snapshot).
// This file implements only the pure selection function for the middle
// layer; it has no I/O and no dependency on database/delivery/smtp so it
// stays trivially unit-testable and reusable from internal/database.
package routing

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// SelectMember deterministically picks one member id from candidates for
// messageID. The same (messageID, candidate set) always yields the same
// result — this is what gives retry stickiness its foundation: callers
// select once at acceptance time and persist the result, never re-call
// this to "re-route" an existing message. candidates need not be sorted;
// SelectMember sorts a copy so member insertion/query order never affects
// the outcome. Returns "" if candidates is empty.
func SelectMember(messageID string, candidates []string) string {
	if len(candidates) == 0 {
		return ""
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	sorted := append([]string(nil), candidates...)
	sort.Strings(sorted)

	sum := sha256.Sum256([]byte(messageID))
	idx := binary.BigEndian.Uint64(sum[:8]) % uint64(len(sorted))
	return sorted[idx]
}
