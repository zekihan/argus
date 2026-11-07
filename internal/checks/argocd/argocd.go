// Package argocd checks Argo CD Applications: sync drift beyond a window that
// depends on whether the app syncs automatically, app-level health, and the
// last sync operation.
package argocd

import (
	"context"
	"sort"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/argus/internal/runner"
)

const Route = "argocd"

var dimensions = []string{"sync", "health", "operation"}

type Application struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		SyncPolicy *struct {
			Automated *struct {
				Enabled *bool `json:"enabled"`
			} `json:"automated"`
		} `json:"syncPolicy"`
	} `json:"spec"`
	Status struct {
		Sync struct {
			Status string `json:"status"`
		} `json:"sync"`
		Health struct {
			Status             string `json:"status"`
			LastTransitionTime string `json:"lastTransitionTime"`
		} `json:"health"`
		OperationState *struct {
			Phase      string `json:"phase"`
			FinishedAt string `json:"finishedAt"`
		} `json:"operationState"`
		Resources []struct {
			Status string `json:"status"`
		} `json:"resources"`
	} `json:"status"`
}

func (a Application) automated() bool {
	p := a.Spec.SyncPolicy
	return p != nil && p.Automated != nil && (p.Automated.Enabled == nil || *p.Automated.Enabled)
}

type Collector interface {
	Collect(ctx context.Context) ([]Application, error)
}

// API lists Applications in the Argo CD namespace (a namespace Role).
type API struct {
	Client    *kube.Client
	Namespace string
}

func (a API) Collect(ctx context.Context) ([]Application, error) {
	return kube.List[Application](ctx, a.Client, "/apis/argoproj.io/v1alpha1/namespaces/"+a.Namespace+"/applications", "")
}

type Check struct {
	config config.ArgoCD
	api    Collector
	seen   *runner.FirstSeen
}

func New(c config.ArgoCD, api Collector) *Check {
	return &Check{config: c, api: api, seen: runner.NewFirstSeen()}
}

func (c *Check) Name() string { return "argocd" }

func (c *Check) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: "argocd/application", Dimensions: dimensions}
	for _, name := range c.config.Expected {
		collection.Items = append(collection.Items, result.Item{Target: name})
	}
	return []result.Collection{collection}
}

func (c *Check) Collect(ctx context.Context, now time.Time) ([]result.Collection, error) {
	apps, err := c.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	return []result.Collection{c.Evaluate(apps, now)}, nil
}

func (c *Check) Evaluate(apps []Application, now time.Time) result.Collection {
	c.seen.Begin(now)
	defer c.seen.End()
	collection := result.Collection{Route: Route, Check: "argocd/application", Dimensions: dimensions, Values: map[string]any{}}
	present := map[string]bool{}
	manualOutOfSync := 0
	for _, app := range apps {
		present[app.Metadata.Name] = true
		item := c.application(app, now)
		if !app.automated() && app.Status.Sync.Status != "Synced" {
			manualOutOfSync++
		}
		collection.Items = append(collection.Items, item)
	}
	var missing []string
	for _, name := range c.config.Expected {
		if !present[name] {
			missing = append(missing, name)
			collection.Items = append(collection.Items, result.Item{Target: name, Values: map[string]any{},
				Results: map[string]string{"sync": result.Reason("missing"), "health": result.NA, "operation": result.NA}})
		}
	}
	sort.Slice(collection.Items, func(i, j int) bool { return collection.Items[i].Target < collection.Items[j].Target })
	collection.Values["manualOutOfSync"] = manualOutOfSync
	collection.Results = map[string]string{"inventory": result.Inventory(missing, nil)}
	return collection
}

func (c *Check) application(app Application, now time.Time) result.Item {
	name := app.Metadata.Name
	automated := app.automated()
	values := map[string]any{"automated": automated, "syncStatus": result.SafeName(app.Status.Sync.Status),
		"healthStatus": result.SafeName(app.Status.Health.Status), "observedSinceSeconds": c.seen.Observed(now).Seconds()}
	outOfSync := 0
	for _, r := range app.Status.Resources {
		if r.Status == "OutOfSync" {
			outOfSync++
		}
	}
	values["outOfSyncResources"] = outOfSync
	results := map[string]string{}
	// Manual apps sit OutOfSync after merges by design (autoSync: false is the
	// readiness gate), so they get a much longer window than automated ones.
	window := time.Duration(c.config.ManualOutOfSyncSeconds) * time.Second
	if automated {
		window = time.Duration(c.config.AutomatedOutOfSyncSeconds) * time.Second
	}
	drifted := app.Status.Sync.Status != "Synced"
	since := c.seen.Active(name+"/sync", drifted, now)
	results["sync"] = result.OK
	if drifted {
		values["outOfSyncObservedSeconds"] = since.Seconds()
		if since > window {
			code := "out_of_sync"
			if app.Status.Sync.Status != "OutOfSync" {
				code = "sync_unknown"
			}
			results["sync"] = result.Reason(code, result.Labeled("for", result.Dur(since.Seconds())), result.Labeled("max", result.Dur(window.Seconds())))
		}
	}
	progressingFor := time.Duration(c.config.ProgressingSeconds) * time.Second
	switch status := app.Status.Health.Status; status {
	case "Healthy", "Suspended":
		results["health"] = result.OK
		c.seen.Active(name+"/progressing", false, now)
	case "Degraded":
		results["health"] = result.Reason("degraded")
	case "Missing":
		results["health"] = result.Reason("missing")
	case "Progressing":
		// From the health lastTransitionTime when Argo CD records one.
		since := c.seen.Active(name+"/progressing", true, now)
		if t, err := kube.ParseTime(app.Status.Health.LastTransitionTime); err == nil {
			since = now.Sub(t)
		}
		results["health"] = result.OK
		if since > progressingFor {
			results["health"] = result.Reason("progressing", result.Labeled("for", result.Dur(since.Seconds())), result.Labeled("max", result.Dur(progressingFor.Seconds())))
		}
	default:
		results["health"] = result.Reason("unknown")
	}
	results["operation"] = result.NA
	if op := app.Status.OperationState; op != nil {
		switch op.Phase {
		case "Failed":
			results["operation"] = result.Reason("failed")
		case "Error":
			results["operation"] = result.Reason("error")
		default:
			results["operation"] = result.OK
		}
		if t, err := kube.ParseTime(op.FinishedAt); err == nil {
			values["lastSync"] = t.Format(time.RFC3339)
		}
	}
	return result.Item{Target: name, Results: result.Set(dimensions, results), Values: values}
}
