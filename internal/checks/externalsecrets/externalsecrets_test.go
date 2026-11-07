package externalsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
)

// Fixtures from platform/external-secrets/tests/test_sync_monitor.py.
var now = time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)

const readyJSON = `{"metadata":{"name":"example","namespace":"apps","generation":2},"spec":{"refreshInterval":"1h"},
"status":{"conditions":[{"type":"Ready","status":"True"}],"syncedResourceVersion":"2-12345","refreshTime":"2026-11-06T00:00:00+00:00"}}`

func decode[T any](t *testing.T, value string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type fakeAPI struct {
	sample Sample
	err    error
}

func (f *fakeAPI) Collect(context.Context) (Sample, error) { return f.sample, f.err }

func healthy(t *testing.T) Sample {
	return Sample{ExternalSecrets: []ExternalSecret{decode[ExternalSecret](t, readyJSON)},
		ClusterExternalSecrets: []Object{decode[Object](t, readyJSON)}, SecretStores: []Object{decode[Object](t, readyJSON)},
		ClusterSecretStores: []Object{decode[Object](t, readyJSON)}}
}

func cfg() config.ExternalSecrets { return config.ExternalSecrets{RefreshGraceSeconds: 600} }

// verdict is /externalsecrets parity across the four collections.
func verdict(t *testing.T, s Sample) (bool, *checktest.Harness) {
	t.Helper()
	h := checktest.New(New(cfg(), &fakeAPI{sample: s}), now).Refresh(t)
	_, es := h.Get(t, ExternalSecretsRoute)
	ok := checktest.Parity(es, "ready", "synced", "refresh", "inventory")
	for _, route := range []string{ClusterExternalSecretsRoute, SecretStoresRoute, ClusterSecretStoresRoute} {
		_, e := h.Get(t, route)
		ok = ok && checktest.Parity(e, "ready", "inventory")
	}
	return ok, h
}

func TestHealthy(t *testing.T) {
	if ok, _ := verdict(t, healthy(t)); !ok {
		t.Fatal("healthy fixture failed")
	}
}

func TestStoresNotReady(t *testing.T) {
	for _, edit := range []func(*Sample){
		func(s *Sample) { s.ClusterSecretStores[0].Status.Conditions = nil },
		func(s *Sample) { s.SecretStores[0].Status.Conditions = nil },
		func(s *Sample) { s.ClusterExternalSecrets[0].Status.Conditions = nil },
	} {
		s := healthy(t)
		edit(&s)
		if ok, _ := verdict(t, s); ok {
			t.Fatal("not-ready store accepted")
		}
	}
}

func TestSecretSyncErrorNeverExposesProviderMessageOrSpec(t *testing.T) {
	s := healthy(t)
	s.ExternalSecrets[0] = decode[ExternalSecret](t, `{"metadata":{"name":"example","namespace":"apps","generation":2},
	"spec":{"refreshInterval":"1h","data":"sensitive-spec-value"},
	"status":{"conditions":[{"type":"Ready","status":"False","reason":"SecretSyncedError","message":"sensitive-provider-message"}],"syncedResourceVersion":"2-12345","refreshTime":"2026-11-06T00:00:00+00:00"}}`)
	ok, h := verdict(t, s)
	_, e := h.Get(t, ExternalSecretsRoute+"/apps/example")
	if ok || e.Results["ready"] != "not_ready" {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, "sensitive-spec-value", "sensitive-provider-message", "SecretSyncedError")
}

func TestGenerationEvidence(t *testing.T) {
	for _, edit := range []func(*ExternalSecret){
		func(es *ExternalSecret) { es.Status.SyncedResourceVersion = "1-12345" },
		func(es *ExternalSecret) { one := int64(1); es.Status.Conditions[0].ObservedGeneration = &one },
		func(es *ExternalSecret) { es.Status.SyncedResourceVersion = "" },
		func(es *ExternalSecret) { es.Metadata.Generation = nil },
	} {
		s := healthy(t)
		edit(&s.ExternalSecrets[0])
		if ok, _ := verdict(t, s); ok {
			t.Fatal("unsynced generation accepted")
		}
	}
}

func TestRefreshBoundary(t *testing.T) {
	for age, want := range map[int]bool{7800: true, 7801: false} {
		s := healthy(t)
		refreshed := now.Add(-time.Duration(age) * time.Second).Format(time.RFC3339)
		s.ExternalSecrets[0].Status.RefreshTime = &refreshed
		if ok, _ := verdict(t, s); ok != want {
			t.Fatal(age, ok)
		}
	}
}

func TestZeroIntervalDoesNotRequireRefreshTime(t *testing.T) {
	for _, interval := range []string{"0", "0s"} {
		s := healthy(t)
		s.ExternalSecrets[0].Spec.RefreshInterval = &interval
		s.ExternalSecrets[0].Status.RefreshTime = nil
		if ok, _ := verdict(t, s); !ok {
			t.Fatal(interval)
		}
	}
}

func TestOnChangeNonzeroIntervalUsesExplicitFreshnessContract(t *testing.T) {
	s := healthy(t)
	old := now.Add(-48 * time.Hour).Format(time.RFC3339)
	s.ExternalSecrets[0].Status.RefreshTime = &old
	if ok, _ := verdict(t, s); ok {
		t.Fatal("refreshPolicy does not relax a nonzero interval")
	}
	zero := "0"
	s.ExternalSecrets[0].Spec.RefreshInterval = &zero
	if ok, _ := verdict(t, s); !ok {
		t.Fatal("zero interval")
	}
	three := int64(3)
	s.ExternalSecrets[0].Metadata.Generation = &three
	if ok, _ := verdict(t, s); ok {
		t.Fatal("new generation without sync accepted")
	}
}

func TestMissingOrInvalidRefresh(t *testing.T) {
	for _, value := range []string{"null", `""`, `"invalid"`, `"2020-01-01T00:00:00"`} {
		s := healthy(t)
		s.ExternalSecrets[0].Status.RefreshTime = decode[*string](t, value)
		if ok, _ := verdict(t, s); ok {
			t.Fatal(value)
		}
	}
}

func TestDurations(t *testing.T) {
	for value, want := range map[string]float64{"1h30m0.5s": 5400.5, "1ms": 0.001, "24h0m0s": 86400, "0": 0} {
		if got, err := Duration(value); err != nil || got != want {
			t.Fatal(value, got, err)
		}
	}
	for _, value := range []string{"-1h", "1d", "junk", "1hmore", ""} {
		if _, err := Duration(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
}

func TestFailedSampleCannotReuseHealthyState(t *testing.T) {
	api := &fakeAPI{sample: healthy(t)}
	h := checktest.New(New(cfg(), api), now).Refresh(t)
	api.err = errors.New("sensitive")
	h.Refresh(t)
	for _, route := range []string{ExternalSecretsRoute, ClusterSecretStoresRoute} {
		if code, _ := h.Get(t, route); code != 503 {
			t.Fatal(route, code)
		}
	}
	h.NoLeak(t, "sensitive")
}

func TestStoreReferenceAndInventory(t *testing.T) {
	s := healthy(t)
	s.ExternalSecrets[0].Spec.SecretStoreRef = &StoreRef{Name: "example", Kind: "ClusterSecretStore"}
	s.ClusterSecretStores[0].Status.Conditions[0].Status = "False"
	c := cfg()
	c.ExpectedExternalSecrets = []config.NamespacedName{{Namespace: "apps", Name: "gone"}}
	c.ExpectedClusterSecretStores = []string{"example", "other"}
	h := checktest.New(New(c, &fakeAPI{sample: s}), now).Refresh(t)
	if _, e := h.Get(t, ExternalSecretsRoute+"/apps/example"); e.Results["store"] != "not_ready(ClusterSecretStore/example)" {
		t.Fatal(e.Results)
	}
	if _, e := h.Get(t, ExternalSecretsRoute); e.Results["inventory"] != "missing(1): apps/gone" {
		t.Fatal(e.Results)
	}
	if code, e := h.Get(t, ClusterSecretStoresRoute+"/other"); code != 503 || e.Results["ready"] != "missing" {
		t.Fatal(e)
	}
	s.ExternalSecrets[0].Spec.SecretStoreRef = &StoreRef{Name: "absent"}
	h = checktest.New(New(cfg(), &fakeAPI{sample: s}), now).Refresh(t)
	if _, e := h.Get(t, ExternalSecretsRoute+"/apps/example"); e.Results["store"] != "missing(SecretStore/absent)" {
		t.Fatal(e.Results)
	}
	forbidden := checktest.New(New(cfg(), &fakeAPI{err: &kube.Error{Code: kube.CodeForbidden, Status: 403}}), now).Refresh(t)
	if _, e := forbidden.Get(t, SecretStoresRoute); e.Results["collection"] != "forbidden" {
		t.Fatal(e.Results)
	}
}
