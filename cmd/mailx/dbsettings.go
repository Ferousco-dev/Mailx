package main

import (
	"context"
	"time"

	"github.com/Ferousco-dev/mailx/internal/database"
)

// Bounded codes for database settings warnings.
const (
	warnPoolExceedsServer = "db_pool_exceeds_server_connections"
	warnFsyncOff          = "db_fsync_off"
	warnFullPageWritesOff = "db_full_page_writes_off"
	warnSynchronousCommit = "db_synchronous_commit_off"
)

// evaluateDBSettings reports settings that break MailX's assumptions. It never
// blocks startup: an operator may legitimately run a small server, but must not do
// so silently.
//
//   - MailX persists a delivery outcome BEFORE acknowledging a queue job, and
//     accepts an email only after its rows are committed. That is only a real
//     guarantee if commits are flushed to disk: fsync, full_page_writes and
//     synchronous_commit must stay on. Memory tuning (shared_buffers, work_mem,
//     max_connections) never needs them off, and the low-memory profile pins them.
//   - The connection pool must fit in the server's non-reserved connections,
//     leaving room for the migrate service and admin CLI commands.
func evaluateDBSettings(s database.ServerSettings) []string {
	var w []string
	if avail := s.MaxConnections - s.ReservedConnections; int(s.PoolMax) > avail {
		w = append(w, warnPoolExceedsServer)
	}
	if !s.Fsync {
		w = append(w, warnFsyncOff)
	}
	if !s.FullPageWrites {
		w = append(w, warnFullPageWritesOff)
	}
	if s.SynchronousCommit == "off" {
		w = append(w, warnSynchronousCommit)
	}
	return w
}

// warnDatabaseSettings logs one bounded warning per problem (numbers and codes
// only). A failure to read the settings is itself only logged.
func warnDatabaseSettings(o obs, db *database.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := db.ServerSettings(ctx)
	if err != nil {
		o.log.Warn("db_settings_unavailable")
		return
	}
	for _, code := range evaluateDBSettings(s) {
		o.log.Warn("db_settings_warning", "code", code, "max_connections", s.MaxConnections,
			"reserved_connections", s.ReservedConnections, "pool_max", s.PoolMax, "synchronous_commit", s.SynchronousCommit)
	}
}
