package pbs

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/go-commons/metrics"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/result"
)

func group(store, kind, id string, last time.Time, guest string) Group {
	g := Group{Store: store, Type: kind, ID: id, Guest: guest}
	if !last.IsZero() {
		g.Last, g.HasLast = last.UTC(), true
	}
	return g
}

func eval(
	t *testing.T,
	cfg config.PBS,
	groups []Group,
	now time.Time,
	reg *metrics.Registry,
) result.Collection {
	t.Helper()
	collections := New(cfg, nil, reg).Evaluate(groups, now)
	if len(collections) != 1 || collections[0].Route != Route {
		t.Fatalf("collections = %+v", collections)
	}
	return collections[0]
}

func item(t *testing.T, c result.Collection, target string) result.Item {
	t.Helper()
	found, ok := c.Find(target)
	if !ok {
		t.Fatalf("missing item %s in %+v", target, c.Items)
	}
	return found
}

func TestFreshnessEdges(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	maxAgeWindow := 26 * time.Hour
	last := now.Add(-maxAgeWindow)
	cfg := config.PBS{MaxAgeSeconds: int(maxAgeWindow / time.Second)}
	reg := metrics.New()
	secret := "argus@pbs!token:super-secret-value" //nolint:gosec // Synthetic test credential, never leaves the fixture.
	collection := eval(t, cfg, []Group{
		group("truenas", "vm", "100", last, "web"),
		group("truenas", "vm", "101", last.Add(-time.Second), ""),
		group("truenas", "vm", "102", now.Add(time.Minute), ""),
		group("truenas", "ct", "200", time.Time{}, secret),
		group("truenas", "host", "pve-1", last, "not a name"),
	}, now, reg)
	if got := item(
		t,
		collection,
		"truenas/vm/100",
	); got.Results["freshness"] != result.OK ||
		got.Values["guest"] != "web" {
		t.Fatalf("exact max age = %+v", got)
	}
	if got := item(t, collection, "truenas/vm/101").Results["freshness"]; !strings.HasPrefix(
		got,
		"backup_stale(",
	) {
		t.Fatalf("just over max = %s", got)
	}
	if got := item(t, collection, "truenas/vm/102").Results["freshness"]; !strings.HasPrefix(
		got,
		"backup_in_future(",
	) {
		t.Fatalf("future = %s", got)
	}
	noBackup := item(t, collection, "truenas/ct/200")
	if noBackup.Results["freshness"] != "no_backup" || noBackup.Values["guest"] != nil {
		t.Fatalf("no backup = %+v", noBackup)
	}
	if item(t, collection, "truenas/host/pve-1").Values["guest"] != nil {
		t.Fatal("unsafe comment became a guest name")
	}
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	text := buf.String()
	if !strings.Contains(
		text,
		`argus_pbs_last_backup_timestamp_seconds{backup_id="100",backup_type="vm",datastore="truenas"}`,
	) ||
		strings.Contains(text, "super-secret") ||
		strings.Contains(text, `backup_id="200"`) {
		t.Fatalf("metrics = %s", text)
	}
}

func TestExpectedExcludeAndDuplicates(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	last := now.Add(-time.Hour)
	cfg := config.PBS{
		MaxAgeSeconds: 93600,
		Expected:      []string{"truenas/vm/100", "truenas/vm/404"},
		Exclude:       []string{"truenas/ct/9"},
	}
	reg := metrics.New()
	collection := eval(t, cfg, []Group{
		group("truenas", "vm", "100", last, ""),
		group("truenas", "vm", "100", last, ""),
		group("truenas", "vm", "7", last, "extra"),
		group("truenas", "ct", "9", last.Add(-100*time.Hour), "retired"),
	}, now, reg)
	if got := item(t, collection, "truenas/vm/100").Results["freshness"]; got != "ambiguous" {
		t.Fatalf("duplicate = %s", got)
	}
	if got := item(t, collection, "truenas/vm/404").Results["freshness"]; got != "missing" {
		t.Fatalf("missing = %s", got)
	}
	if collection.Results["inventory"] == result.OK {
		t.Fatalf("inventory = %s", collection.Results["inventory"])
	}
	if _, ok := collection.Find("truenas/ct/9"); ok {
		t.Fatal("excluded group was reported")
	}
	var buf bytes.Buffer
	reg.WritePrometheus(&buf)
	if strings.Contains(buf.String(), `backup_id="9"`) ||
		strings.Contains(buf.String(), "retired") {
		t.Fatalf("excluded group was exported: %s", buf.String())
	}
	open := eval(
		t,
		config.PBS{MaxAgeSeconds: 93600},
		[]Group{group("truenas", "vm", "7", last, "")},
		now,
		nil,
	)
	if open.Results["inventory"] != result.OK {
		t.Fatalf("discovered group without an expected list = %s", open.Results["inventory"])
	}
}

func TestMaxAgeOverride(t *testing.T) {
	now := time.Date(2026, time.November, 23, 12, 0, 0, 0, time.UTC)
	weekly := 8*24*time.Hour + 2*time.Hour
	cfg := config.PBS{
		MaxAgeSeconds: 26 * 3600,
		MaxAgeOverrides: []config.PBSMaxAgeOverride{
			{Match: "truenas/vm/401*", MaxAgeSeconds: int(weekly / time.Second)},
			{Match: "truenas/vm/402*", MaxAgeSeconds: int(weekly / time.Second)},
		},
	}
	collection := eval(t, cfg, []Group{
		group("truenas", "vm", "40101", now.Add(-weekly), ""),
		group("truenas", "vm", "40102", now.Add(-weekly-time.Second), ""),
		group("truenas", "vm", "40205", now.Add(-7*24*time.Hour), ""),
		group("truenas", "vm", "104", now.Add(-26*time.Hour), ""),
		group("truenas", "vm", "105", now.Add(-26*time.Hour-time.Second), ""),
		group("truenas", "ct", "116", now.Add(-time.Hour), ""),
	}, now, nil)
	if got := item(t, collection, "truenas/vm/40101").Results["freshness"]; got != result.OK {
		t.Fatalf("weekly limit = %s", got)
	}
	if got := item(t, collection, "truenas/vm/40102").Results["freshness"]; !strings.HasPrefix(
		got,
		"backup_stale(",
	) {
		t.Fatalf("past weekly limit = %s", got)
	}
	if got := item(t, collection, "truenas/vm/40205").Results["freshness"]; got != result.OK {
		t.Fatalf("second weekly glob = %s", got)
	}
	if got := item(t, collection, "truenas/vm/104").Results["freshness"]; got != result.OK {
		t.Fatalf("nightly limit = %s", got)
	}
	if got := item(t, collection, "truenas/vm/105").Results["freshness"]; !strings.HasPrefix(
		got,
		"backup_stale(",
	) {
		t.Fatalf("past nightly limit = %s", got)
	}
	if got := item(t, collection, "truenas/ct/116").Results["freshness"]; got != result.OK {
		t.Fatalf("unmatched group = %s", got)
	}
}
