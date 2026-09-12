package events

import (
	"strings"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func obj(kind, id, hash string) model.SchemaObject {
	return model.SchemaObject{Kind: kind, Identity: id, DefinitionHash: hash, Definition: kind + " " + id}
}

func find(evs []model.Event, kind, object string) *model.Event {
	for i := range evs {
		if evs[i].Kind == kind && evs[i].Object == object {
			return &evs[i]
		}
	}
	return nil
}

func timePtrEq(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func TestDerive_schemaChanges(t *testing.T) {
	prevAt := time.Date(2026, 8, 12, 9, 2, 0, 0, time.UTC)
	now := time.Date(2026, 8, 12, 9, 17, 0, 0, time.UTC)

	prev := []model.SchemaObject{
		obj("index", "public.orders.orders_customer_idx", "h1"),
		obj("table", "public.orders", "t1"),
		obj("column", "public.orders.status", "c1"),
	}
	cur := &model.Context{
		CollectedAt: now,
		Settings:    &model.Settings{Overrides: map[string]string{"work_mem": "64MB"}},
		Schema: &model.SchemaFingerprint{Objects: []model.SchemaObject{
			obj("table", "public.orders", "t1"),         // unchanged
			obj("column", "public.orders.status", "c2"), // type changed (hash differs)
			obj("index", "public.orders.new_idx", "h9"), // created
			// orders_customer_idx dropped
		}},
	}
	prevSettings := map[string]string{"work_mem": "4MB"}

	evs := Derive(cur, prev, prevSettings, prevAt)

	if e := find(evs, "schema.index_dropped", "public.orders.orders_customer_idx"); e == nil {
		t.Error("expected index_dropped")
	} else {
		if e.OccurredAfter == nil || !e.OccurredAfter.Equal(prevAt) || e.OccurredBefore == nil || !e.OccurredBefore.Equal(now) {
			t.Errorf("dropped event should carry the (prevAt, now) window, got %+v", e)
		}
		if e.Confidence >= 1.0 {
			t.Error("inferred schema change must have confidence < 1.0")
		}
	}
	if find(evs, "schema.index_created", "public.orders.new_idx") == nil {
		t.Error("expected index_created")
	}
	if find(evs, "schema.column_type_changed", "public.orders.status") == nil {
		t.Error("expected column_type_changed")
	}
	if e := find(evs, "config.changed", "work_mem"); e == nil || e.Before != "4MB" || e.After != "64MB" {
		t.Errorf("expected config.changed work_mem 4MB->64MB, got %+v", e)
	}
}

func TestDerive_firstRunNoSchemaEvents(t *testing.T) {
	cur := &model.Context{CollectedAt: time.Now(), Schema: &model.SchemaFingerprint{Objects: []model.SchemaObject{obj("table", "public.x", "h")}}}
	if evs := Derive(cur, nil, nil, time.Now()); len(evs) != 0 {
		t.Errorf("no prior fingerprint should yield no schema events, got %+v", evs)
	}
}

func TestDerive_lifecycleHasRealTimestamps(t *testing.T) {
	prevAt := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	reset := time.Date(2026, 8, 12, 9, 14, 0, 0, time.UTC)
	cur := &model.Context{
		CollectedAt: time.Date(2026, 8, 12, 9, 17, 0, 0, time.UTC),
		Server:      model.ServerInfo{Database: "app"},
		Window:      model.Window{StatsResetAt: &reset},
	}
	e := find(Derive(cur, nil, nil, prevAt), "stats.reset", "app")
	if e == nil || e.Confidence != 1.0 {
		t.Fatalf("stats.reset should fire with confidence 1.0, got %+v", e)
	}
	if e.OccurredAfter == nil || !e.OccurredAfter.Equal(reset) {
		t.Error("stats.reset should carry the real reset timestamp")
	}
}

func TestDerive_redactsSecretSettings(t *testing.T) {
	cur := &model.Context{CollectedAt: time.Now(), Settings: &model.Settings{Overrides: map[string]string{
		"ssl_cert_file": "/new/path.crt",
	}}}
	prev := map[string]string{"ssl_cert_file": "/old/path.crt"}
	e := find(Derive(cur, nil, prev, time.Now()), "config.changed", "ssl_cert_file")
	if e == nil {
		t.Fatal("expected config.changed for ssl_cert_file")
	}
	if e.Before == "/old/path.crt" || e.After == "/new/path.crt" {
		t.Errorf("ssl file paths must be redacted, got before=%q after=%q", e.Before, e.After)
	}
}

// Derive must be deterministic: Go map iteration order is not, so the raw
// loop over curByID/prevByID/settings produced events in a different order on
// every run — two runs over identical state must be byte-identical in the
// report, the stored snapshot and the AI payload (bug_report.md Bug 6).
func TestDerive_deterministicOrder(t *testing.T) {
	prevAt := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 12, 9, 5, 0, 0, time.UTC)

	prevSchema := []model.SchemaObject{
		obj("index", "public.a.i1", "h1"), obj("index", "public.b.i2", "h2"),
		obj("table", "public.c", "t1"), obj("table", "public.d", "t2"),
	}
	cur := &model.Context{
		CollectedAt: now,
		Schema: &model.SchemaFingerprint{Objects: []model.SchemaObject{
			obj("index", "public.e.i3", "h3"), obj("index", "public.f.i4", "h4"),
			obj("table", "public.c", "t1"), obj("table", "public.g", "t5"),
		}},
		Settings: &model.Settings{Overrides: map[string]string{"work_mem": "64MB", "shared_buffers": "1GB"}},
	}
	prevSettings := map[string]string{"work_mem": "32MB", "max_connections": "200"}

	first := Derive(cur, prevSchema, prevSettings, prevAt)
	if len(first) < 6 {
		t.Fatalf("fixture must produce several events, got %d", len(first))
	}
	// Re-derive many times: identical state, identical order, every time.
	// (Events carry time pointers — compare values, not pointer identity.)
	sameEvent := func(a, b model.Event) bool {
		return a.Kind == b.Kind && a.Object == b.Object && a.Before == b.Before && a.After == b.After &&
			a.Confidence == b.Confidence &&
			timePtrEq(a.OccurredAfter, b.OccurredAfter) && timePtrEq(a.OccurredBefore, b.OccurredBefore)
	}
	for i := 0; i < 200; i++ {
		again := Derive(cur, prevSchema, prevSettings, prevAt)
		if len(again) != len(first) {
			t.Fatalf("run %d: %d events, first run had %d", i, len(again), len(first))
		}
		for j := range again {
			if !sameEvent(again[j], first[j]) {
				t.Fatalf("run %d: event %d differs:\n got %+v\nwant %+v", i, j, again[j], first[j])
			}
		}
	}
	// And within each family (lifecycle, schema, config — appended in that
	// order by Derive) the order is the canonical (kind, object) sort.
	var schemaEvs, configEvs []model.Event
	for _, e := range first {
		switch {
		case strings.HasPrefix(e.Kind, "schema."):
			schemaEvs = append(schemaEvs, e)
		case e.Kind == "config.changed":
			configEvs = append(configEvs, e)
		}
	}
	for i := 1; i < len(schemaEvs); i++ {
		a, b := schemaEvs[i-1], schemaEvs[i]
		if a.Kind > b.Kind || (a.Kind == b.Kind && a.Object > b.Object) {
			t.Errorf("schema events not sorted by (kind, object): %q/%q before %q/%q", a.Kind, a.Object, b.Kind, b.Object)
		}
	}
	for i := 1; i < len(configEvs); i++ {
		if configEvs[i-1].Object > configEvs[i].Object {
			t.Errorf("config events not sorted by object: %q before %q", configEvs[i-1].Object, configEvs[i].Object)
		}
	}
}
