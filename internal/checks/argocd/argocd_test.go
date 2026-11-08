package argocd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
)

var now = time.Date(2026, 11, 7, 15, 0, 0, 0, time.UTC)

func app(t *testing.T, name string, automated bool, sync, health, transition, phase string) Application {
	policy := `{"syncOptions":["Prune=false"]}`
	if automated {
		policy = `{"automated":{"prune":true}}`
	}
	body := `{"metadata":{"name":"` + name + `","namespace":"argocd"},"spec":{"syncPolicy":` + policy + `},
	"status":{"sync":{"status":"` + sync + `"},"health":{"status":"` + health + `","lastTransitionTime":"` + transition + `"},
	"operationState":{"phase":"` + phase + `","finishedAt":"2026-11-07T11:48:09Z","message":"token=argo-secret"},
	"resources":[{"kind":"Deployment","status":"OutOfSync"},{"kind":"Service","status":"Synced"}]}}`
	var a Application
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

type fakeAPI struct{ apps []Application }

func (f *fakeAPI) Collect(context.Context) ([]Application, error) { return f.apps, nil }

func cfg() config.ArgoCD {
	c := config.ArgoCD{Namespace: "argocd", Expected: []string{"auto", "manual"}}
	full := config.Config{Checks: config.Checks{ArgoCD: &c}}
	full.ApplyDefaults()
	return *full.Checks.ArgoCD
}

func results(t *testing.T, check *Check, apps []Application, at time.Time, name string) map[string]string {
	item, ok := check.Evaluate(apps, at).Find(name)
	if !ok {
		t.Fatal(name)
	}
	return item.Results
}

func TestSyncWindowsDependOnAutomation(t *testing.T) {
	check := New(cfg(), nil)
	apps := []Application{app(t, "auto", true, "OutOfSync", "Healthy", "", "Succeeded"), app(t, "manual", false, "OutOfSync", "Healthy", "", "Succeeded")}
	for _, offset := range []time.Duration{0, 15 * time.Minute} {
		if r := results(t, check, apps, now.Add(offset), "auto"); r["sync"] != "ok" {
			t.Fatal(offset, r)
		}
	}
	if r := results(t, check, apps, now.Add(16*time.Minute), "auto"); r["sync"] != "out_of_sync(for 16m, max 15m)" {
		t.Fatal(r)
	}
	if r := results(t, check, apps, now.Add(6*24*time.Hour), "manual"); r["sync"] != "ok" {
		t.Fatal("manual app paged within its window", r)
	}
	if r := results(t, check, apps, now.Add(7*24*time.Hour+time.Minute), "manual"); r["sync"] != "out_of_sync(for 7d0h, max 7d0h)" {
		t.Fatal(r)
	}
	collection := check.Evaluate(apps, now.Add(7*24*time.Hour+2*time.Minute))
	if collection.Values["manualOutOfSync"] != 1 {
		t.Fatal(collection.Values)
	}
	item, _ := collection.Find("auto")
	if item.Values["outOfSyncResources"] != 1 {
		t.Fatal(item.Values)
	}
	// A sync resets the window.
	apps[0].Status.Sync.Status = "Synced"
	results(t, check, apps, now.Add(8*24*time.Hour), "auto")
	apps[0].Status.Sync.Status = "OutOfSync"
	if r := results(t, check, apps, now.Add(8*24*time.Hour+time.Minute), "auto"); r["sync"] != "ok" {
		t.Fatal(r)
	}
}

func TestUnknownSyncFailsWithoutAWindow(t *testing.T) {
	check := New(cfg(), nil)
	for _, status := range []string{"Unknown", ""} {
		apps := []Application{app(t, "manual", false, status, "Healthy", "", "Succeeded")}
		if r := results(t, check, apps, now, "manual"); r["sync"] != "sync_unknown" {
			t.Fatal(status, r)
		}
	}
}

func TestHealthAndOperation(t *testing.T) {
	check := New(cfg(), nil)
	for _, tc := range []struct {
		health, transition, phase string
		dim, want                 string
	}{
		{"Degraded", "", "Succeeded", "health", "degraded"},
		{"Missing", "", "Succeeded", "health", "missing"},
		{"Progressing", "2026-11-07T14:50:00Z", "Succeeded", "health", "ok"},
		{"Progressing", "2026-11-07T14:40:00Z", "Succeeded", "health", "progressing(for 20m, max 15m)"},
		{"Unknown", "", "Succeeded", "health", "unknown"},
		{"Suspended", "", "Succeeded", "health", "ok"},
		{"Healthy", "", "Failed", "operation", "failed"},
		{"Healthy", "", "Error", "operation", "error"},
		{"Healthy", "", "Running", "operation", "ok"},
	} {
		apps := []Application{app(t, "auto", true, "Synced", tc.health, tc.transition, tc.phase)}
		if r := results(t, check, apps, now, "auto"); r[tc.dim] != tc.want {
			t.Fatal(tc, r)
		}
	}
}

func TestMissingApplicationAndHygiene(t *testing.T) {
	h := checktest.New(New(cfg(), &fakeAPI{apps: []Application{app(t, "auto", true, "Synced", "Healthy", "", "Succeeded")}}), now).Refresh(t)
	if code, e := h.Get(t, Route+"/manual"); code != 503 || e.Results["sync"] != "missing" {
		t.Fatal(e)
	}
	if code, e := h.Get(t, Route+"/auto"); code != 200 || e.Values["lastSync"] != "2026-11-07T11:48:09Z" {
		t.Fatal(e)
	}
	h.NoLeak(t, "argo-secret")
}
