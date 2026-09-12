package model

import "testing"

// TestPGVersionString pins the server_version_num → header rendering for both
// encodings. Regression guard for the pre-10 bug where the PATCH level was
// printed as the minor (90603 rendered "postgres 9.3" on a 9.6.3 server —
// bug_report.md Bug 3).
func TestPGVersionString(t *testing.T) {
	cases := []struct {
		num  int
		want string
		note string
	}{
		{0, "postgres", "unknown version"},
		{90603, "postgres 9.6", "9.6.3 — three-part encoding, minor at (num/100)%100"},
		{90500, "postgres 9.5", "9.5.0"},
		{90401, "postgres 9.4", "9.4.1"},
		{90024, "postgres 9.0", "9.0.24 — patch never leaks into the minor"},
		{100004, "postgres 10.4", "PG10+ two-part encoding"},
		{100000, "postgres 10.0", "PG 10.0"},
		{160003, "postgres 16.3", "modern major"},
		{180001, "postgres 18.1", "current major line"},
	}
	for _, tc := range cases {
		if got := PGVersionString(tc.num); got != tc.want {
			t.Errorf("PGVersionString(%d) = %q, want %q (%s)", tc.num, got, tc.want, tc.note)
		}
	}
}

// TestPGVersionStringPatchDistinct confirms two patch levels of one pre-10
// minor render identically (patch is deliberately not shown) while two
// DIFFERENT minors render differently — the property the old code broke.
func TestPGVersionStringPatchDistinct(t *testing.T) {
	if PGVersionString(90603) != PGVersionString(90624) {
		t.Error("patch level must not change the rendered version")
	}
	if PGVersionString(90603) == PGVersionString(90303) {
		t.Error("9.6.x and 9.3.x must render differently")
	}
}

func TestServerInfoShortVersion(t *testing.T) {
	s := ServerInfo{VersionNum: 90603}
	if got := s.ShortVersion(); got != "postgres 9.6" {
		t.Errorf("ShortVersion() = %q, want %q", got, "postgres 9.6")
	}
}
