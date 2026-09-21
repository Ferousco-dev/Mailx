package database

import (
	"context"
	"fmt"
	"strconv"
)

// ServerSettings are the PostgreSQL server settings MailX depends on. They are
// read once at startup so an operator who tuned the server for a small host
// (fewer connections, less memory) gets a clear warning if the tuning would
// weaken MailX's guarantees or starve its connection pool.
type ServerSettings struct {
	MaxConnections      int
	ReservedConnections int // superuser_reserved_connections
	Fsync               bool
	FullPageWrites      bool
	SynchronousCommit   string // on, off, local, remote_write, remote_apply
	// PoolMax is the size of this process's connection pool.
	PoolMax int32
}

// ServerSettings reads the settings above with one query.
func (db *DB) ServerSettings(ctx context.Context) (ServerSettings, error) {
	var maxConns, reserved, fsync, fpw string
	var s ServerSettings
	err := db.pool.QueryRow(ctx, `SELECT current_setting('max_connections'), current_setting('superuser_reserved_connections'),
		current_setting('fsync'), current_setting('full_page_writes'), current_setting('synchronous_commit')`).
		Scan(&maxConns, &reserved, &fsync, &fpw, &s.SynchronousCommit)
	if err != nil {
		return ServerSettings{}, fmt.Errorf("database: read server settings: %w", err)
	}
	if s.MaxConnections, err = strconv.Atoi(maxConns); err != nil {
		return ServerSettings{}, fmt.Errorf("database: unexpected max_connections %q", maxConns)
	}
	if s.ReservedConnections, err = strconv.Atoi(reserved); err != nil {
		return ServerSettings{}, fmt.Errorf("database: unexpected superuser_reserved_connections %q", reserved)
	}
	s.Fsync, s.FullPageWrites = fsync == "on", fpw == "on"
	s.PoolMax = db.pool.Config().MaxConns
	return s, nil
}
