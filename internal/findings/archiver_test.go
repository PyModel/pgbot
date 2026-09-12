package findings

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestWalArchiving(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	recent := time.Now().Add(-time.Minute)

	// Most recent attempt was a failure → critical (self-managed).
	failing := &model.Context{Archiver: &model.Archiver{LastArchivedTime: &past, LastFailedTime: &recent, FailedCount: 5}}
	f := has(Compute(failing), "archiving_failing")
	if f == nil || f.Severity != model.SeverityCritical {
		t.Fatalf("expected critical archiving_failing, got %+v", f)
	}

	// Same on RDS → downgraded to info + a can't-see-backups caveat (A15-0 rule 2).
	rds := &model.Context{Server: model.ServerInfo{Provider: "rds"}, Archiver: &model.Archiver{LastArchivedTime: &past, LastFailedTime: &recent, FailedCount: 5}}
	f = has(Compute(rds), "archiving_failing")
	if f == nil || f.Severity != model.SeverityInfo || len(f.Caveats) == 0 {
		t.Errorf("on rds, archiving_failing must be info + caveat, got %+v", f)
	}

	// A stale failure OLDER than the last success must not fire (no baseline needed).
	stale := &model.Context{Archiver: &model.Archiver{LastArchivedTime: &recent, LastFailedTime: &past, FailedCount: 5}}
	if has(Compute(stale), "archiving_failing") != nil {
		t.Error("a failure older than the last success must not fire critical")
	}

	// Compound: broken archiving + large pg_wal → extra evidence line.
	big := int64(20 << 30)
	compound := &model.Context{Archiver: &model.Archiver{LastArchivedTime: &past, LastFailedTime: &recent}, WAL: &model.WAL{DirBytes: &big}}
	f = has(Compute(compound), "archiving_failing")
	joined := ""
	for _, e := range f.Evidence {
		joined += e
	}
	if !strings.Contains(joined, "pg_wal is now") {
		t.Errorf("compound archiving+WAL should add a pg_wal evidence line: %v", f.Evidence)
	}

	// archive_mode=off, wal_level=replica, no replication → archiving_disabled (warn).
	off := &model.Context{Archiver: &model.Archiver{}, Settings: &model.Settings{Params: map[string]string{"archive_mode": "off", "wal_level": "replica"}}}
	if d := has(Compute(off), "archiving_disabled"); d == nil || d.Severity != model.SeverityWarn {
		t.Errorf("archive_mode=off should warn, got %+v", d)
	}
}

// The run-over-run failed_count delta must fire archiving_failing even when the
// timestamp says currently-succeeding (the intermittent-failure case).
func TestWalArchiving_deltaTrigger(t *testing.T) {
	recent := time.Now().Add(-time.Minute)
	c := &model.Context{
		Archiver: &model.Archiver{LastArchivedTime: &recent, FailedCount: 100}, // succeeding now
		Deltas:   &model.Deltas{Changes: []model.Delta{{ID: "archiver.failed_count", Before: 80, After: 100}}},
	}
	f := has(Compute(c), "archiving_failing")
	if f == nil || f.Severity != model.SeverityCritical {
		t.Fatalf("20 new failures since baseline must fire critical even while succeeding now, got %+v", f)
	}
	if !strings.Contains(f.Evidence[0], "new archiving failures") {
		t.Errorf("evidence should cite the delta: %v", f.Evidence)
	}
}

// parsePGDuration must handle the unit-suffixed strings current_setting()
// actually returns ("5min", "1h", "0"), which the old strconv.Atoi path could
// not parse at all.
func TestParsePGDuration(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"0", 0, true},
		{"30s", 30 * time.Second, true},
		{"5min", 5 * time.Minute, true},
		{"1h", time.Hour, true},
		{"1h 30min", 90 * time.Minute, true},
		{"2d", 48 * time.Hour, true},
		{"250ms", 250 * time.Millisecond, true},
		{"100us", 100 * time.Microsecond, true},
		{" 5min ", 5 * time.Minute, true},
		{"-5min", -5 * time.Minute, true},
		{"", 0, false},
		{"5", 5 * time.Second, true}, // bare number = base unit (seconds); the server prints "0" for disabled
		{"min", 0, false},            // unit without a number
		{"5weeks", 0, false},         // unknown unit
		{"5 min x", 0, false},
	}
	// The base unit is per-GUC in PostgreSQL (ms for statement_timeout, s for
	// archive_timeout, min for autovacuum_naptime): a bare number must scale by
	// the caller's base, or a raw pg_settings value like "30000" (ms) silently
	// parses as 30000 SECONDS — the same wrong-branch family the Atoi fix closed.
	if d, ok := parsePGDuration("30000", time.Millisecond); !ok || d != 30*time.Second {
		t.Errorf("parsePGDuration(30000, ms base) = %v, %v; want 30s, true", d, ok)
	}
	if d, ok := parsePGDuration("5", time.Minute); !ok || d != 5*time.Minute {
		t.Errorf("parsePGDuration(5, min base) = %v, %v; want 5m, true", d, ok)
	}
	for _, tc := range cases {
		got, ok := parsePGDuration(tc.in, time.Second)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parsePGDuration(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// archiveStallThreshold is max(archive_timeout×3, 1h). Before the fix the
// setting was unparseable, so every server got the 1h floor — a false
// archiving_stalled critical for anyone with archive_timeout above ~20min.
func TestArchiveStallThreshold(t *testing.T) {
	mk := func(timeout string) *model.Context {
		return &model.Context{Settings: &model.Settings{Params: map[string]string{"archive_timeout": timeout}}}
	}
	cases := []struct {
		timeout string
		want    time.Duration
	}{
		{"", time.Hour},        // unknown → floor
		{"0", time.Hour},       // disabled → floor
		{"5min", time.Hour},    // 15min < floor → floor
		{"1h", 3 * time.Hour},  // 3h > floor
		{"8h", 24 * time.Hour}, // a long timeout widens the window
	}
	for _, tc := range cases {
		if got := archiveStallThreshold(mk(tc.timeout)); got != tc.want {
			t.Errorf("archiveStallThreshold(archive_timeout=%q) = %v; want %v", tc.timeout, got, tc.want)
		}
	}
}

// End to end: with archive_timeout=8h the stall window is 24h, so a segment
// last archived 2h ago while WAL flows must NOT fire archiving_stalled. The old
// dead parse used the 1h floor and fired a false critical here.
func TestWalArchiving_stalledRespectsArchiveTimeout(t *testing.T) {
	flow := 1024.0
	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	base := func(timeout string) *model.Context {
		return &model.Context{
			Archiver: &model.Archiver{LastArchivedTime: &twoHoursAgo},
			WAL:      &model.WAL{BytesPerSec: &flow},
			Settings: &model.Settings{Params: map[string]string{"archive_mode": "on", "archive_timeout": timeout}},
		}
	}
	if f := has(Compute(base("8h")), "archiving_stalled"); f != nil {
		t.Errorf("archive_timeout=8h: 2h without an archive is inside the 24h window, must not fire: %+v", f)
	}
	if f := has(Compute(base("5min")), "archiving_stalled"); f == nil {
		t.Error("archive_timeout=5min: 2h without an archive exceeds the 1h floor, must fire")
	}
}

// Never-archived server (regression, bug_report.md Bug 4): archive_mode on, WAL
// flowing, last_archived_time NULL and no failure recorded — the signature of an
// archive_command that HANGS rather than returns. Neither the failing arm (needs
// a failure signal) nor the stalled arm (needs a success timestamp) can fire, so
// this state used to pass in silence while pg_wal filled.
func TestWalArchiving_neverArchived(t *testing.T) {
	flow := 2048.0
	up := int64(3 * time.Hour.Seconds()) // older than the 1h floor
	mk := func() *model.Context {
		return &model.Context{
			Server:   model.ServerInfo{UptimeSeconds: up},
			Archiver: &model.Archiver{FailedCount: 0}, // last_archived_time NULL, never failed
			WAL:      &model.WAL{BytesPerSec: &flow},
			Settings: &model.Settings{Params: map[string]string{"archive_mode": "on"}},
		}
	}
	f := has(Compute(mk()), "archiving_stalled")
	if f == nil {
		t.Fatal("hung archive_command on a never-archived server must fire archiving_stalled")
	}
	if f.Severity != model.SeverityCritical {
		t.Errorf("severity = %s, want critical", f.Severity)
	}
	if !strings.Contains(f.Title, "never succeeded") {
		t.Errorf("title should say this is a never-archived state: %q", f.Title)
	}

	// A server younger than the threshold hasn't had time to archive yet.
	young := mk()
	young.Server.UptimeSeconds = int64(10 * time.Minute.Seconds())
	if has(Compute(young), "archiving_stalled") != nil {
		t.Error("a server younger than the stall threshold must not fire")
	}

	// A recent pg_stat_reset NULLs last_archived_time too — must not be read as
	// "never archived".
	reset := mk()
	recent := time.Now().Add(-10 * time.Minute)
	reset.Window.StatsResetAt = &recent
	if has(Compute(reset), "archiving_stalled") != nil {
		t.Error("a recent stats reset must not be misread as never-archived")
	}

	// No WAL flowing → nothing to archive → silence.
	idle := mk()
	idle.WAL.BytesPerSec = nil
	if has(Compute(idle), "archiving_stalled") != nil {
		t.Error("no WAL flowing means nothing to archive — must stay silent")
	}

	// archive_mode off takes the archiving_disabled path instead.
	off := mk()
	off.Settings.Params["archive_mode"] = "off"
	if has(Compute(off), "archiving_stalled") != nil {
		t.Error("archive_mode=off must not fire the stall finding")
	}
}
