package main

import (
	"reflect"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/database"
)

func TestEvaluateDBSettings(t *testing.T) {
	ok := database.ServerSettings{MaxConnections: 100, ReservedConnections: 3, Fsync: true, FullPageWrites: true, SynchronousCommit: "on", PoolMax: 10}
	for name, tc := range map[string]struct {
		mut  func(*database.ServerSettings)
		want []string
	}{
		"defaults are quiet":                 {func(*database.ServerSettings) {}, nil},
		"the low-memory profile is quiet":    {func(s *database.ServerSettings) { s.MaxConnections = 20 }, nil},
		"remote_apply and local are durable": {func(s *database.ServerSettings) { s.SynchronousCommit = "remote_apply" }, nil},
		"pool larger than the server allows": {func(s *database.ServerSettings) { s.MaxConnections = 12 }, []string{warnPoolExceedsServer}},
		"exactly fits":                       {func(s *database.ServerSettings) { s.MaxConnections = 13 }, nil},
		"fsync off":                          {func(s *database.ServerSettings) { s.Fsync = false }, []string{warnFsyncOff}},
		"full_page_writes off":               {func(s *database.ServerSettings) { s.FullPageWrites = false }, []string{warnFullPageWritesOff}},
		"synchronous_commit off":             {func(s *database.ServerSettings) { s.SynchronousCommit = "off" }, []string{warnSynchronousCommit}},
		"everything wrong": {func(s *database.ServerSettings) {
			*s = database.ServerSettings{MaxConnections: 5, PoolMax: 10, SynchronousCommit: "off"}
		}, []string{warnPoolExceedsServer, warnFsyncOff, warnFullPageWritesOff, warnSynchronousCommit}},
	} {
		s := ok
		tc.mut(&s)
		if got := evaluateDBSettings(s); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v want %v", name, got, tc.want)
		}
	}
}
