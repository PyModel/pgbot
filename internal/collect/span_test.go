package collect

import (
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/model"
)

// rateWindow is the divisor discipline for counter rates: a collector's OWN
// measured span when the runner stamped one, the shared window otherwise
// (bug_report.md N4 — io/wal/iostats sampled outside the window used to be
// divided by the window, inflating every rate by the phase lead+lag).
func TestSampledRateWindow(t *testing.T) {
	fallback := time.Second
	if got := (sampled{}).rateWindow(fallback); got != fallback {
		t.Errorf("unstamped sampled must fall back to the window, got %v", got)
	}
	stamped := sampled{Span: 3 * time.Second}
	if got := stamped.rateWindow(fallback); got != 3*time.Second {
		t.Errorf("stamped span must win, got %v", got)
	}
	// A non-positive stamp is refused, not blindly trusted.
	broken := sampled{Span: -time.Second}
	if got := broken.rateWindow(fallback); got != fallback {
		t.Errorf("negative span must fall back, got %v", got)
	}
	// Health's samples ARE the window: identical stamps yield the window.
	hA := time.Now()
	hB := hA.Add(time.Second)
	if got := (sampled{AtA: hA, AtB: hB, Span: hB.Sub(hA)}).rateWindow(time.Second); got != time.Second {
		t.Errorf("health-equivalent stamps must yield the window, got %v", got)
	}
}

// A counter divided by its OWN measured span: sample deltas of 2000 reads over
// a measured 20s span (the runner window was 10s) must render 100/s — not the
// 200/s the shared-window divisor produced. Mirrors the existing
// TestIOStats_assembleRatesAndLatency fixture.
func TestIOStatsUsesOwnSpanNotSharedWindow(t *testing.T) {
	caps := conn.Capabilities{VersionNum: 180000}
	row := func(reads int64, readT float64, writes int64) ioStatRow {
		return ioStatRow{BackendType: "client backend", Object: "relation", Context: "normal", Reads: reads, ReadTime: readT, Writes: writes}
	}
	a := ioStatsReading{rows: []ioStatRow{row(1000, 500, 10)}, ioTiming: true}
	b := ioStatsReading{rows: []ioStatRow{row(3000, 1500, 30)}, ioTiming: true}

	// Phase-stamped: A at t0, B at t0+20s; the runner window was 10s.
	c := &model.Context{}
	iostatsCollector{}.Assemble(c, caps, sampled{A: a, B: b, Span: 20 * time.Second}, 10*time.Second, Options{})
	if io := c.IOStats; io == nil || io.ReadsPerSec == nil || *io.ReadsPerSec != 100 {
		t.Errorf("reads/s = %v, want 100 (2000 reads over the MEASURED 20s span): %+v",
			deref(io.ReadsPerSec), io)
	}
}

// Health's own transaction footprint must vanish from its rates: the sampler's
// N successful polls and its own sample-A query are N+1 commits inside the
// window; its M aborted polls are M rollbacks (bug_report.md minor + PR#1).
func TestHealthSubtractsOwnTransactions(t *testing.T) {
	a := healthSample{XactCommit: 1000, XactRollback: 10}
	// A completely idle database where pgbot ran 20 sampler polls (17 ok, 3
	// aborted) plus health's own sample-A commit: +18 commits, +3 rollbacks.
	b := healthSample{
		XactCommit:   1000 + 17 + 1, // sampler + health's own A query
		XactRollback: 10 + 3,        // aborted polls
	}
	c := &model.Context{}
	healthCollector{}.Assemble(c, conn.Capabilities{}, sampled{
		A: a, B: b, Span: time.Second,
		OwnTxns: 17, OwnTxnFails: 3,
	}, time.Second, Options{})
	h := c.Health
	if h == nil || h.Exactness != model.ExactnessSampled {
		t.Fatalf("want sampled health: %+v", h)
	}
	if h.TPS == nil || *h.TPS != 0 {
		t.Errorf("an idle database must read 0 TPS after subtracting pgbot's own %d transactions, got %v",
			21, deref(h.TPS))
	}
	if h.CommitsPerSec == nil || *h.CommitsPerSec != 0 {
		t.Errorf("own commits must cancel, got %v", deref(h.CommitsPerSec))
	}
	if h.RollbackRatio != nil && *h.RollbackRatio != 0.01 {
		t.Errorf("rollback ratio must reflect only the workload (10/1010 ≈ 0.0099), got %v", *h.RollbackRatio)
	}
}

func deref(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}
