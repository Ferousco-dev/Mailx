package database

import (
	"context"
	"testing"
)

// Against a real PostgreSQL: the settings are read, sane, and the pool size is this process's.
func TestServerSettingsAreReadFromTheServer(t *testing.T) {
	db := newTestDB(t)
	s, err := db.ServerSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxConnections < 1 || s.ReservedConnections < 0 || s.SynchronousCommit == "" || s.PoolMax < 1 {
		t.Fatalf("%+v", s)
	}
	// The test server keeps MailX's durability assumptions.
	if !s.Fsync || !s.FullPageWrites || s.SynchronousCommit == "off" {
		t.Fatalf("test PostgreSQL has weakened durability settings: %+v", s)
	}
}
