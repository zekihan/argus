package kube

import (
	"math"
	"testing"
	"time"
)

func TestParseTimeRequiresZone(t *testing.T) {
	for _, value := range []string{"2026-11-06T00:00:00Z", "2026-11-06T00:00:00.5+03:00", "2026-11-06T00:00:00-01:00"} {
		if _, err := ParseTime(value); err != nil {
			t.Fatal(value, err)
		}
	}
	if got, _ := ParseTime("2026-11-06T03:00:00+03:00"); !got.Equal(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(got)
	}
	for _, value := range []string{"", "invalid", "2026-11-06T00:00:00", "2026-11-06", "2026-11-06 00:00:00Z", "2026-13-06T00:00:00Z"} {
		if _, err := ParseTime(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
}

func TestQuantity(t *testing.T) {
	for value, want := range map[string]float64{"12": 12, "500m": 0.5, "123456789n": 0.123456789, "14279684Ki": 14279684 * 1024, "2Gi": 2 << 30, "1e3": 1000, "1.5k": 1500} {
		if got, err := Quantity(value); err != nil || math.Abs(got-want) > 1e-9*math.Max(1, want) {
			t.Fatal(value, got, err)
		}
	}
	for _, value := range []string{"", "x", "1Zi", "--1"} {
		if _, err := Quantity(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
}

func TestConditions(t *testing.T) {
	conditions := []Condition{{Type: "Ready", Status: "False"}, {Type: "Synced", Status: "True"}}
	if ConditionTrue(conditions, "Ready") || !ConditionTrue(conditions, "Synced") || ConditionTrue(conditions, "Missing") {
		t.Fatal("condition lookup")
	}
	meta := Metadata{Owners: []Owner{{Kind: "CronJob", UID: "u", Controller: true}}}
	if !OwnedBy(meta, "CronJob", "u") || OwnedBy(meta, "CronJob", "") || OwnedBy(meta, "Job", "u") {
		t.Fatal("owner lookup")
	}
}
