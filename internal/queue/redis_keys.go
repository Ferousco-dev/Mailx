package queue

import "strings"

// redisKeys is the namespaced key layout for one RedisQueue. Everything
// lives under mailx:queue:<namespace>: so tests, environments, and future
// queues sharing one Redis instance cannot collide.
//
// available: ZSET member=jobID score=AvailableAt(unix ms) — claimable now
// or in the future.
// claimed: ZSET member=jobID score=lease-expiry(unix ms) — currently owned;
// an expired score is what makes a claim reclaimable.
// job:<id>: HASH — the Job fields plus the current owning token (0 = none).
// tokenseq: STRING counter — INCR gives each claim a fresh, unique token.
type redisKeys struct {
	available string
	claimed   string
	tokenSeq  string
	jobPrefix string
}

func newRedisKeys(namespace string) redisKeys {
	base := "mailx:queue:" + namespace + ":"
	return redisKeys{
		available: base + "available",
		claimed:   base + "claimed",
		tokenSeq:  base + "tokenseq",
		jobPrefix: base + "job:",
	}
}

func (k redisKeys) job(id string) string {
	return k.jobPrefix + id
}

func validNamespace(ns string) bool {
	if ns == "" {
		return false
	}
	for _, r := range ns {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return !strings.ContainsAny(ns, "\r\n\x00")
}
