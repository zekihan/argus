package result

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 7, 14, 11, 28, 0, time.UTC)

func volumes() Collection {
	return Collection{Route: "longhorn/volumes", Check: "longhorn/volume", Dimensions: []string{"volume", "backup", "replicas"},
		Items: []Item{
			{Target: "immich/library", Results: map[string]string{"volume": OK, "backup": Reason("backup_stale", Labeled("last", Dur(111600)), Labeled("max", Dur(93600))), "replicas": OK},
				Values: map[string]any{"replicasHealthy": 2}},
			{Target: "mailvault/state", Results: map[string]string{"volume": OK, "backup": OK, "replicas": NA}},
		}}
}

func TestReasonCodesAndParameters(t *testing.T) {
	if got := Reason("backup_stale", Labeled("last", Dur(111600)), Labeled("max", Dur(93600))); got != "backup_stale(last 1d7h, max 1d2h)" {
		t.Fatal(got)
	}
	for seconds, want := range map[float64]string{0: "0s", 59: "59s", 5400: "1h30m", 172800: "2d0h", -3600: "1h0m", 90: "1m"} {
		if got := Duration(seconds); got != want {
			t.Fatal(seconds, got)
		}
	}
	if got := Reason("condition_false", Name("Bad Type\"<script>")); got != "condition_false(Bad_Type__script_)" {
		t.Fatal(got)
	}
	for _, bad := range []string{"Bad", "has space", "", "x(y)"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("malformed code accepted", bad)
				}
			}()
			Reason(bad)
		}()
	}
	if got := ProbeText("a\x00b\ncé"+strings.Repeat("x", 300), 10); got != "a?b c?xxxx" {
		t.Fatal(got)
	}
}

func TestItemAndAggregateEnvelopes(t *testing.T) {
	c := volumes()
	item, _ := c.Find("immich/library")
	envelope := c.ItemEnvelope(item, OK, now, now)
	if envelope.Healthy || envelope.Check != "longhorn/volume" || envelope.Target != "immich/library" ||
		envelope.Summary != "backup: backup_stale(last 1d7h, max 1d2h)" || envelope.Results["collection"] != OK {
		t.Fatalf("%+v", envelope)
	}
	healthy, _ := c.Find("mailvault/state")
	if e := c.ItemEnvelope(healthy, OK, now, now); !e.Healthy || e.Summary != OK {
		t.Fatalf("%+v", e)
	}
	if e := c.ItemEnvelope(healthy, Reason("stale"), now, now); e.Healthy || e.Summary != "collection: stale" {
		t.Fatalf("%+v", e)
	}
	aggregate := c.Aggregate(OK, now, now)
	if aggregate.Healthy || aggregate.Check != "longhorn/volumes" || aggregate.Results["backup"] != "1 failing: immich/library" ||
		aggregate.Results["replicas"] != OK || aggregate.Results["volume"] != OK || aggregate.Results["inventory"] != OK ||
		aggregate.Items["mailvault/state"] != OK || aggregate.Values["unhealthyItems"] != 1 {
		t.Fatalf("%+v", aggregate)
	}
	pending := c.Aggregate(Pending, time.Time{}, now)
	if pending.Healthy || pending.ObservedAt != nil || pending.Results["collection"] != Pending {
		t.Fatalf("%+v", pending)
	}
	body, _ := json.Marshal(aggregate)
	if !strings.Contains(string(body), `"observedAt":"2026-11-07T14:11:28Z"`) {
		t.Fatal(string(body))
	}
}

func TestAggregateCapsFailingItems(t *testing.T) {
	c := Collection{Route: "nodes", Check: "node", Dimensions: []string{"ready"}}
	for i := range 100 {
		c.Items = append(c.Items, Item{Target: strings.Repeat("n", 40) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Results: map[string]string{"ready": "not_ready"}})
	}
	value := c.Aggregate(OK, now, now).Results["ready"]
	if len(value) > summaryLimit+32 || !strings.HasPrefix(value, "100 failing: ") || !strings.Contains(value, "more") {
		t.Fatal(len(value), value)
	}
}

func TestRefineRunsAtResponseTime(t *testing.T) {
	c := Collection{Route: "x", Check: "x", Dimensions: []string{"freshness"}, Items: []Item{{Target: "a", Results: map[string]string{"freshness": OK},
		Refine: func(at time.Time, item *Item) {
			if at.After(now) {
				item.Results["freshness"] = Reason("expired")
			}
		}}}}
	if c.Aggregate(OK, now, now).Results["freshness"] != OK || c.Aggregate(OK, now, now.Add(time.Second)).Results["freshness"] != "1 failing: a" {
		t.Fatal("refine not applied")
	}
	if c.Items[0].Results["freshness"] != OK {
		t.Fatal("refine mutated the stored sample")
	}
}

func TestInventoryAndSet(t *testing.T) {
	if Inventory(nil, nil) != OK || Inventory([]string{"b", "a"}, []string{"c"}) != "missing(2): a, b; unexpected(1): c" {
		t.Fatal(Inventory([]string{"b", "a"}, []string{"c"}))
	}
	if got := Set([]string{"a", "b"}, map[string]string{"a": OK}); got["b"] != "unknown" {
		t.Fatal("forgotten dimension passed", got)
	}
}
