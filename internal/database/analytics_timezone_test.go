package database

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestAnalyticsTimeseriesUsesUTCRegardlessOfDatabaseTimezone(t *testing.T) {
	dsn, err := url.Parse(baseTestDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("timezone", "Asia/Kathmandu")
	dsn.RawQuery = query.Encode()
	t.Setenv("MAILX_TEST_DATABASE_URL", dsn.String())
	db := newTestDB(t)
	ctx := context.Background()
	var timezone string
	if err := db.pool.QueryRow(ctx, `SHOW timezone`).Scan(&timezone); err != nil || timezone != "Asia/Kathmandu" {
		t.Fatalf("test timezone: %s %v", timezone, err)
	}
	tn := newTestTenant(t, db)
	msg, err := db.InsertMessage(ctx, sampleNewMessage(t, tn.ID))
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	insertEvent(t, db, tn.ID, msg.ID, EventQueued, day.Add(15*time.Minute))
	insertEvent(t, db, tn.ID, msg.ID, EventQueued, day.Add(20*time.Hour))
	for _, interval := range []string{"day", "hour"} {
		buckets, err := db.AnalyticsTimeseries(ctx, tn.ID, day, day.Add(24*time.Hour), interval)
		if err != nil {
			t.Fatal(err)
		}
		if interval == "day" {
			if len(buckets) != 1 || !buckets[0].Timestamp.Equal(day) || buckets[0].Counts.Queued != 2 {
				t.Fatalf("day buckets follow local timezone: %+v", buckets)
			}
		} else if len(buckets) != 2 || !buckets[0].Timestamp.Equal(day) || !buckets[1].Timestamp.Equal(day.Add(20*time.Hour)) {
			t.Fatalf("hour buckets follow local timezone: %+v", buckets)
		}
	}
}
