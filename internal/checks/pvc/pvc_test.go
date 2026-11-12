package pvc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
)

var now = time.Now().UTC()

func ptr(n uint64) *uint64 { return &n }
func cfg() config.PVCs {
	return config.PVCs{StorageClasses: []string{"longhorn"}, ExpectedNodes: []string{"worker"}, UsedPercent: 80, MaxStatsAgeSeconds: 300}
}
func sample(t *testing.T) Sample {
	t.Helper()
	var claim Claim
	if err := json.Unmarshal([]byte(`{"metadata":{"namespace":"db","name":"pgdata","uid":"claim-a"},"spec":{"storageClassName":"longhorn","volumeName":"pv-a"},"status":{"phase":"Bound"}}`), &claim); err != nil {
		t.Fatal(err)
	}
	var n Node
	n.Metadata.Name = "worker"
	return Sample{Claims: []Claim{claim}, Nodes: []Node{n}, Stats: []VolumeStats{{PVCRef: &config.NamespacedName{Namespace: "db", Name: "pgdata"}, Time: now.Format(time.RFC3339), UsedBytes: ptr(80), CapacityBytes: ptr(100)}}}
}

type fakeAPI struct {
	sample Sample
	err    error
}

func (f fakeAPI) Collect(context.Context) (Sample, error) { return f.sample, f.err }

func TestThresholdFailsItemAndAggregateHTTP(t *testing.T) {
	for _, used := range []uint64{0, 79, 80, 81, 100} {
		s := sample(t)
		s.Stats[0].UsedBytes = ptr(used)
		h := checktest.New(New(cfg(), fakeAPI{sample: s}), now).Refresh(t)
		want := 200
		if used > 80 {
			want = 503
		}
		for _, route := range []string{"pvcs", "pvcs/db/pgdata"} {
			code, e := h.Get(t, route)
			if code != want || e.Healthy != (want == 200) {
				t.Fatalf("used=%d route=%s code=%d body=%+v", used, route, code, e)
			}
		}
	}
}

func TestUnavailableAndInventoryFailClosed(t *testing.T) {
	for name, mutate := range map[string]func(*Sample){
		"missing stats":         func(s *Sample) { s.Stats = nil },
		"stale":                 func(s *Sample) { s.Stats[0].Time = now.Add(-301 * time.Second).Format(time.RFC3339) },
		"future":                func(s *Sample) { s.Stats[0].Time = now.Add(61 * time.Second).Format(time.RFC3339) },
		"invalid time":          func(s *Sample) { s.Stats[0].Time = "credential-shaped-secret" },
		"missing used":          func(s *Sample) { s.Stats[0].UsedBytes = nil },
		"missing capacity":      func(s *Sample) { s.Stats[0].CapacityBytes = nil },
		"zero capacity":         func(s *Sample) { s.Stats[0].CapacityBytes = ptr(0) },
		"used exceeds capacity": func(s *Sample) { s.Stats[0].UsedBytes = ptr(101) },
		"unbound":               func(s *Sample) { s.Claims[0].Status.Phase = "Pending" },
		"empty inventory":       func(s *Sample) { s.Claims = nil },
		"missing node":          func(s *Sample) { s.Nodes = nil },
		"unexpected node":       func(s *Sample) { s.Nodes[0].Metadata.Name = "new-worker" },
	} {
		t.Run(name, func(t *testing.T) {
			s := sample(t)
			mutate(&s)
			h := checktest.New(New(cfg(), fakeAPI{sample: s}), now).Refresh(t)
			code, e := h.Get(t, Route)
			if code != 503 {
				t.Fatal(e)
			}
			data, _ := json.Marshal(e)
			if strings.Contains(string(data), "credential-shaped-secret") {
				t.Fatal("leaked input", string(data))
			}
		})
	}
}

func TestDiscoveryIncludesRedisAndNonBackupVolumes(t *testing.T) {
	s := sample(t)
	for _, name := range []string{"redis", "other-volume"} {
		claim := s.Claims[0]
		claim.Metadata.Name = name
		s.Claims = append(s.Claims, claim)
	}
	claim := s.Claims[0]
	claim.Metadata.Name = "smb"
	claim.Spec.StorageClassName = "smb"
	s.Claims = append(s.Claims, claim)
	c := New(cfg(), nil).Evaluate(s, now)
	if len(c.Items) != 3 {
		t.Fatal(c.Items)
	}
	if c.Aggregate("ok", now, now).Healthy {
		t.Fatal("unmeasured Redis/other volumes passed")
	}
}

func TestExpectedPVCAndMultipleMounts(t *testing.T) {
	s := sample(t)
	conf := cfg()
	conf.Expected = []config.NamespacedName{{Namespace: "db", Name: "missing"}}
	c := New(conf, nil).Evaluate(s, now)
	if c.Aggregate("ok", now, now).Healthy {
		t.Fatal("missing expected PVC passed")
	}
	for _, used := range []uint64{50, 95} {
		duplicate := s.Stats[0]
		duplicate.UsedBytes = ptr(used)
		s.Stats = append(s.Stats, duplicate)
	}
	stale := s.Stats[0]
	stale.Time = now.Add(-time.Hour).Format(time.RFC3339)
	s.Stats = append(s.Stats, stale)
	c = New(cfg(), nil).Evaluate(s, now)
	item, _ := c.Find("db/pgdata")
	if item.Values["usedPercent"] != float64(95) || !strings.HasPrefix(item.Results["usage"], "high(") {
		t.Fatal(item)
	}
}

func TestCollectionErrorsAndExpiredSample(t *testing.T) {
	for _, failure := range []string{kube.CodeForbidden, kube.CodeUnauthorized, kube.CodeTimeout, kube.CodeInvalid} {
		h := checktest.New(New(cfg(), fakeAPI{err: &kube.Error{Code: failure}}), now).Refresh(t)
		code, e := h.Get(t, Route)
		if code != 503 || e.Results["collection"] != failure {
			t.Fatal(code, e)
		}
	}
	h := checktest.New(New(cfg(), fakeAPI{sample: sample(t)}), now).Refresh(t)
	if code, e := h.Runner.Lookup(Route, now.Add(301*time.Second)); code != 503 {
		t.Fatal(e)
	}
}

func TestAllowedUnmountedNeedsPositiveAttachmentInventory(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		allow, collected, attached bool
		want                       bool
	}{
		{"permitted detached", true, true, false, true},
		{"unlisted detached", false, true, false, false},
		{"not collected", true, false, false, false},
		{"attachment exists", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sample(t)
			s.Stats = nil
			s.AttachmentsCollected = tc.collected
			if tc.attached {
				var a Attachment
				a.Spec.Source.PersistentVolumeName = "pv-a"
				s.Attachments = []Attachment{a}
			}
			conf := cfg()
			if tc.allow {
				conf.AllowUnmounted = []config.NamespacedName{{Namespace: "db", Name: "pgdata"}}
			}
			c := New(conf, nil).Evaluate(s, now)
			if e := c.Aggregate("ok", now, now); e.Healthy != tc.want {
				t.Fatal(e)
			}
		})
	}
	s := sample(t)
	s.Claims = nil
	s.AttachmentsCollected = true
	conf := cfg()
	conf.AllowUnmounted = []config.NamespacedName{{Namespace: "db", Name: "pgdata"}}
	if e := New(conf, nil).Evaluate(s, now).Aggregate("ok", now, now); e.Healthy {
		t.Fatal("missing allowed claim passed", e)
	}
}

func TestRemountCacheExpiresAndKeepsHighMeasurementFailing(t *testing.T) {
	for _, used := range []uint64{50, 81} {
		s := sample(t)
		s.Stats[0].UsedBytes = ptr(used)
		check := New(cfg(), nil)
		check.Evaluate(s, now)
		s.Stats = nil
		c := check.Evaluate(s, now.Add(2*time.Minute))
		if e := c.Aggregate("ok", now, now.Add(2*time.Minute)); e.Healthy != (used <= 80) {
			t.Fatal(used, e)
		}
		if e := c.Aggregate("ok", now, now.Add(301*time.Second)); e.Healthy {
			t.Fatal("response-time expiry failed", e)
		}
		if e := check.Evaluate(s, now.Add(301*time.Second)).Aggregate("ok", now, now.Add(301*time.Second)); e.Healthy {
			t.Fatal("cached measurement did not expire", e)
		}
		if len(check.cached) != 0 {
			t.Fatal("expired cache retained")
		}
	}
	// A recreated PVC cannot inherit the old claim's measurement.
	check := New(cfg(), nil)
	s := sample(t)
	check.Evaluate(s, now)
	s.Stats = nil
	s.Claims[0].Metadata.UID = "claim-b"
	if e := check.Evaluate(s, now).Aggregate("ok", now, now); e.Healthy {
		t.Fatal("reused name inherited old stats", e)
	}
}

func TestDetachedHighCacheAndRawBlockApplicability(t *testing.T) {
	conf := cfg()
	conf.AllowUnmounted = []config.NamespacedName{{Namespace: "db", Name: "pgdata"}}
	check := New(conf, nil)
	s := sample(t)
	s.AttachmentsCollected = true
	s.Stats[0].UsedBytes = ptr(81)
	check.Evaluate(s, now)
	s.Stats = nil
	if e := check.Evaluate(s, now.Add(time.Minute)).Aggregate("ok", now, now.Add(time.Minute)); e.Healthy {
		t.Fatal("detached high measurement passed", e)
	}
	s.Claims[0].Spec.VolumeMode = "Block"
	c := New(cfg(), nil).Evaluate(s, now)
	item, _ := c.Find("db/pgdata")
	if item.Results["usage"] != "n/a" || !c.Aggregate("ok", now, now).Healthy {
		t.Fatal(item)
	}
}

func TestPermittedAttachmentStatsGraceIsBounded(t *testing.T) {
	for _, age := range []time.Duration{20 * time.Second, 80 * time.Second, 300 * time.Second, 301 * time.Second} {
		s := sample(t)
		s.Stats = nil
		s.AttachmentsCollected = true
		var a Attachment
		a.Spec.Source.PersistentVolumeName = "pv-a"
		a.Metadata.CreationTimestamp = now.Add(-age).Format(time.RFC3339)
		s.Attachments = []Attachment{a}
		conf := cfg()
		conf.AllowUnmounted = []config.NamespacedName{{Namespace: "db", Name: "pgdata"}}
		c := New(conf, nil).Evaluate(s, now)
		if e := c.Aggregate("ok", now, now); e.Healthy != (age < 300*time.Second) {
			t.Fatal(age, e)
		}
		if e := c.Aggregate("ok", now, now.Add(301*time.Second)); e.Healthy {
			t.Fatal("attachment grace did not expire at response time", e)
		}
		// The exception must never apply to a normal pgdata claim.
		if e := New(cfg(), nil).Evaluate(s, now).Aggregate("ok", now, now); e.Healthy {
			t.Fatal("unlisted claim got grace", e)
		}
	}
}

func TestAttachmentGraceNeverHidesBadOrHighMeasurements(t *testing.T) {
	conf := cfg()
	conf.AllowUnmounted = []config.NamespacedName{{Namespace: "db", Name: "pgdata"}}
	for _, stamp := range []string{"", "bad", now.Add(time.Minute).Format(time.RFC3339)} {
		s := sample(t)
		s.Stats = nil
		s.AttachmentsCollected = true
		var a Attachment
		a.Spec.Source.PersistentVolumeName = "pv-a"
		a.Metadata.CreationTimestamp = stamp
		s.Attachments = []Attachment{a}
		if e := New(conf, nil).Evaluate(s, now).Aggregate("ok", now, now); e.Healthy {
			t.Fatal("bad attachment timestamp passed", stamp, e)
		}
	}
	for _, bad := range []string{"high", "invalid", "future", "cached high"} {
		s := sample(t)
		s.AttachmentsCollected = true
		var a Attachment
		a.Spec.Source.PersistentVolumeName = "pv-a"
		a.Metadata.CreationTimestamp = now.Add(-20 * time.Second).Format(time.RFC3339)
		s.Attachments = []Attachment{a}
		check := New(conf, nil)
		switch bad {
		case "high":
			s.Stats[0].UsedBytes = ptr(81)
		case "invalid":
			s.Stats[0].Time = "bad"
		case "future":
			s.Stats[0].Time = now.Add(2 * time.Minute).Format(time.RFC3339)
		case "cached high":
			s.Stats[0].UsedBytes = ptr(81)
			check.Evaluate(s, now)
			s.Stats = nil
		}
		if e := check.Evaluate(s, now).Aggregate("ok", now, now); e.Healthy {
			t.Fatal(bad, e)
		}
	}
}

func TestInvalidSampleCannotBeHiddenByLaterStaleSample(t *testing.T) {
	for _, attached := range []bool{false, true} {
		for _, future := range []bool{false, true} {
			for _, reverse := range []bool{false, true} {
				s := sample(t)
				s.AttachmentsCollected = true
				if attached {
					var a Attachment
					a.Spec.Source.PersistentVolumeName = "pv-a"
					a.Metadata.CreationTimestamp = now.Add(-30 * time.Second).Format(time.RFC3339)
					s.Attachments = []Attachment{a}
				}
				stale := s.Stats[0]
				stale.Time = now.Add(-time.Hour).Format(time.RFC3339)
				bad := s.Stats[0]
				if future {
					bad.Time = now.Add(5 * time.Minute).Format(time.RFC3339)
				} else {
					bad.UsedBytes = ptr(200)
				}
				s.Stats = []VolumeStats{bad, stale}
				if reverse {
					s.Stats = []VolumeStats{stale, bad}
				}
				conf := cfg()
				conf.AllowUnmounted = []config.NamespacedName{{Namespace: "db", Name: "pgdata"}}
				c := New(conf, nil).Evaluate(s, now)
				item, _ := c.Find("db/pgdata")
				if item.Results["usage"] != "metrics_invalid" || c.Aggregate("ok", now, now).Healthy {
					t.Fatalf("attached=%v future=%v reverse=%v item=%+v", attached, future, reverse, item)
				}
			}
		}
	}
}
