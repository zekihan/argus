// Package pgbackrest is backrestwatch's pgBackRest backup completion freshness
// check. Evaluate and repositoryIdentity are ported unchanged; this file
// replaces backrestwatch's monitor loop with a ConfigMap ledger whose writes
// are conditional and converge by re-evaluation after a conflict.
package pgbackrest

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/argus/internal/runner"
	"github.com/zekihan/argus/internal/state"
)

const (
	Route = "pgbackrest"
	// writeAttempts bounds conditional writes per sample; each failed write is
	// followed by a read-back and a re-evaluation.
	writeAttempts = 3
)

var dimensions = []string{"freshness", "evidence", "destination", "stanza", "state"}

// Sample is one consistent read of the objects the check evaluates.
type Sample struct {
	Clusters []kube.Cluster
	Jobs     []kube.Job
	CronJobs []kube.CronJob
}

// Collector reads a Sample.
type Collector interface {
	Collect(ctx context.Context) (Sample, error)
}

// Persistence is the conditional ledger store.
type Persistence interface {
	Load(ctx context.Context) (state.Ledger, error)
	Save(ctx context.Context, ledger state.Ledger) error
}

// API collects from Kubernetes: PostgresClusters cluster-wide (for inventory
// drift), backup Jobs and CronJobs only in the inventory namespaces.
type API struct {
	Client     *kube.Client
	Namespaces []string
}

func (a API) Collect(ctx context.Context) (Sample, error) {
	var sample Sample
	var err error
	if sample.Clusters, err = kube.List[kube.Cluster](ctx, a.Client, "/apis/"+kube.ClusterAPI+"/postgresclusters", ""); err != nil {
		return Sample{}, err
	}
	for _, namespace := range a.Namespaces {
		jobs, err := kube.List[kube.Job](ctx, a.Client, "/apis/batch/v1/namespaces/"+namespace+"/jobs", kube.Prefix+"pgbackrest-backup")
		if err != nil {
			return Sample{}, err
		}
		crons, err := kube.List[kube.CronJob](ctx, a.Client, "/apis/batch/v1/namespaces/"+namespace+"/cronjobs", kube.Prefix+"pgbackrest-backup=scheduled")
		if err != nil {
			return Sample{}, err
		}
		sample.Jobs, sample.CronJobs = append(sample.Jobs, jobs...), append(sample.CronJobs, crons...)
	}
	return sample, nil
}

// Namespaces lists the inventory namespaces in a stable order.
func Namespaces(c config.PGBackRest) []string {
	seen := map[string]bool{}
	var out []string
	for _, cluster := range c.Clusters {
		if !seen[cluster.Namespace] {
			seen[cluster.Namespace] = true
			out = append(out, cluster.Namespace)
		}
	}
	sort.Strings(out)
	return out
}

// published is the last result shown for a repository: only evaluations whose
// ledger was persisted are published.
type published struct {
	view  view
	valid bool
}

type view struct {
	result      Result
	entry       state.Entry
	evaluatedAt time.Time
	stanza      string
}

type Check struct {
	config   config.PGBackRest
	api      Collector
	store    Persistence
	readOnly bool
	logger   *slog.Logger

	// ledger holds persisted successes plus invalidations not yet persisted;
	// persisted is the ledger last read from or written to the store.
	ledger     state.Ledger
	persisted  state.Ledger
	loaded     bool
	views      map[string]published
	unexpected []string
	missing    []string
}

// New returns the check. readOnly evaluates against the stored ledger without writing.
func New(c config.PGBackRest, api Collector, store Persistence, readOnly bool, logger *slog.Logger) *Check {
	return &Check{config: c, api: api, store: store, readOnly: readOnly, logger: logger, views: map[string]published{}}
}

func (c *Check) Name() string { return "pgbackrest" }

func (c *Check) keys() []string {
	var keys []string
	for _, cluster := range c.config.Clusters {
		for repo := range cluster.Repos {
			keys = append(keys, cluster.Namespace+"/"+cluster.Name+"/"+repo)
		}
	}
	sort.Strings(keys)
	return keys
}

func (c *Check) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: "pgbackrest/repository", Dimensions: dimensions}
	for _, key := range c.keys() {
		collection.Items = append(collection.Items, result.Item{Target: key})
	}
	return []result.Collection{collection}
}

func stateCode(err error) string {
	switch {
	case errors.Is(err, state.ErrInvalid):
		return "invalid"
	case errors.Is(err, state.ErrMissing):
		return "missing"
	case errors.Is(err, state.ErrTooLarge):
		return "too_large"
	default:
		return runner.Code(err)
	}
}

func (c *Check) Collect(ctx context.Context, clock func() time.Time) ([]result.Collection, error) {
	// Read the stored ledger on every sample, so a ledger rolled back by Argo
	// CD's compare-then-apply, or repaired by an operator, converges on the
	// next poll rather than at the next write.
	stored, err := c.store.Load(ctx)
	if errors.Is(err, state.ErrInvalid) || errors.Is(err, state.ErrMissing) {
		// A corrupt or unknown document fails closed and is never overwritten:
		// nothing is evaluated or written until a read succeeds.
		c.loaded, c.ledger, c.persisted = false, nil, nil
		c.logger.Error("pgBackRest ledger unavailable", "code", stateCode(err))
		return c.render(stateCode(err)), nil
	}
	if err != nil {
		// An unreadable ConfigMap (403, timeout, outage) fails the collection
		// like any other API read; pending invalidations stay in memory.
		return nil, err
	}
	switch {
	case !c.loaded:
		c.ledger, c.persisted, c.loaded = stored, stored, true
	case !state.Equal(stored, c.persisted):
		c.persisted = stored
		c.ledger = adopt(stored, c.ledger)
	}
	sample, err := c.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	// As in backrestwatch, evaluate after collecting: completions observed
	// during the read are not in the future.
	now := clock()
	results, next, err := Evaluate(c.config, sample, c.ledger, now)
	if err != nil {
		if next != nil {
			c.ledger = retainInvalidations(c.ledger, next)
		}
		return nil, runner.Fail("rejected")
	}
	// Write only when the result differs from what is stored.
	if c.readOnly || state.Equal(next, c.persisted) {
		c.ledger = next
		c.publish(sample, results, next, now)
		return c.render(result.OK), nil
	}
	code := "unpersisted"
	for attempt := 0; attempt < writeAttempts; attempt++ {
		err := c.store.Save(ctx, next)
		if err == nil {
			c.ledger, c.persisted = next, next
			c.publish(sample, results, next, now)
			return c.render(result.OK), nil
		}
		if errors.Is(err, state.ErrTooLarge) {
			code = "too_large"
			break
		}
		// 409, timeout or reset: the write may or may not have landed, or another
		// writer changed the ledger. Re-read, adopt it and re-evaluate this sample.
		stored, loadErr := c.store.Load(ctx)
		if loadErr != nil {
			if errors.Is(loadErr, state.ErrInvalid) || errors.Is(loadErr, state.ErrMissing) {
				c.loaded, c.ledger, c.persisted = false, nil, nil
				c.logger.Error("pgBackRest ledger unavailable", "code", stateCode(loadErr))
				return c.render(stateCode(loadErr)), nil
			}
			break
		}
		c.persisted = stored
		c.ledger = adopt(stored, c.ledger)
		results, next, err = Evaluate(c.config, sample, c.ledger, now)
		if err != nil {
			if next != nil {
				c.ledger = retainInvalidations(c.ledger, next)
			}
			return nil, runner.Fail("rejected")
		}
		if state.Equal(next, stored) {
			c.ledger = next
			c.publish(sample, results, next, now)
			return c.render(result.OK), nil
		}
	}
	// A failed write must not undo an observed disappearance or destination
	// change. Retain its safety boundary, but not newly observed successes.
	c.ledger = retainInvalidations(c.ledger, next)
	c.logger.Error("pgBackRest ledger write failed", "code", code)
	return c.render(code), nil
}

// adopt takes the stored ledger after a failed write, keeping any barrier this
// process holds that is newer than the stored one. The in-memory ledger only
// ever holds persisted successes plus invalidations not yet persisted
// (retainInvalidations), so a pending invalidation is not lost when the
// destination reverted before it could be written.
func adopt(stored, current state.Ledger) state.Ledger {
	out := make(state.Ledger, len(stored))
	for key, entry := range stored {
		out[key] = entry
	}
	for key, entry := range current {
		old, ok := stored[key]
		if !entry.NotBefore.IsZero() && (!ok || entry.NotBefore.After(old.NotBefore)) {
			out[key] = entry
		}
	}
	return out
}

func retainInvalidations(previous, next state.Ledger) state.Ledger {
	pending := make(state.Ledger, len(next))
	for key, entry := range next {
		old := previous[key]
		if !entry.LastSuccess.IsZero() && old.UID == entry.UID && old.Created.Equal(entry.Created) &&
			old.RepositoryID == entry.RepositoryID && old.NotBefore.Equal(entry.NotBefore) {
			entry.LastSuccess, entry.Source = old.LastSuccess, old.Source
		} else {
			entry.LastSuccess, entry.Source = time.Time{}, ""
		}
		pending[key] = entry
	}
	return pending
}

func stanza(cluster kube.Cluster, repo string) string {
	for _, status := range cluster.Status.PGBackRest.Repos {
		if status.Name == repo {
			if status.StanzaCreated {
				return result.OK
			}
			return result.Reason("not_created")
		}
	}
	return result.Reason("not_created")
}

// publish records results whose ledger is durable (or, read-only, current).
func (c *Check) publish(sample Sample, results map[string]Result, ledger state.Ledger, now time.Time) {
	clusters := map[string]kube.Cluster{}
	for _, cluster := range sample.Clusters {
		clusters[cluster.Metadata.Namespace+"/"+cluster.Metadata.Name] = cluster
	}
	views := map[string]published{}
	c.missing, c.unexpected = nil, nil
	for key, r := range results {
		parts := strings.Split(key, "/")
		clusterKey := parts[0] + "/" + parts[1]
		switch r.Reason {
		case "Backup repository is not in the expected inventory":
			c.unexpected = append(c.unexpected, key)
			continue
		case "PostgresCluster is not in the expected inventory":
			c.unexpected = append(c.unexpected, clusterKey)
			continue
		case "Expected PostgresCluster is missing", "Expected backup repository is missing":
			c.missing = append(c.missing, key)
		}
		v := view{result: r, entry: ledger[key], evaluatedAt: now, stanza: result.NA}
		if cluster, ok := clusters[clusterKey]; ok && hasRepo(cluster, parts[2]) {
			v.stanza = stanza(cluster, parts[2])
		}
		views[key] = published{view: v, valid: true}
	}
	c.views = views
}

func (c *Check) render(stateResult string) []result.Collection {
	collection := result.Collection{Route: Route, Check: "pgbackrest/repository", Dimensions: dimensions,
		Results: map[string]string{"inventory": result.Inventory(c.missing, c.unexpected)}}
	if stateResult != result.OK && len(c.views) == 0 {
		collection.Results["inventory"] = result.Pending
	}
	for _, key := range c.keys() {
		p := c.views[key]
		item := result.Item{Target: key}
		if p.valid {
			item = p.view.item(key, c.readOnly)
		} else {
			item.Results = map[string]string{"freshness": result.Pending, "evidence": result.Pending, "destination": result.Pending, "stanza": result.Pending}
			item.Values = map[string]any{}
		}
		item.Results["state"] = stateResult
		collection.Items = append(collection.Items, item)
	}
	return []result.Collection{collection}
}

func (v view) item(key string, readOnly bool) result.Item {
	r := v.result
	results := map[string]string{"freshness": result.OK, "evidence": result.OK, "destination": result.OK, "stanza": v.stanza}
	values := map[string]any{"maxAgeSeconds": r.MaxAgeSeconds}
	if r.ClusterUID != "" {
		values["clusterUID"] = r.ClusterUID
	}
	if readOnly {
		values["readOnly"] = true
	}
	if !v.entry.NotBefore.IsZero() {
		values["notBefore"] = v.entry.NotBefore.UTC().Format(time.RFC3339Nano)
	}
	if r.LastSuccess != nil {
		values["lastSuccess"] = r.LastSuccess.UTC().Format(time.RFC3339Nano)
		values["source"] = r.Source
	}
	switch r.Reason {
	case "":
	case "Expected PostgresCluster is missing":
		results["destination"], results["freshness"], results["evidence"], results["stanza"] = result.Reason("cluster_missing"), result.Reason("unknown"), result.NA, result.NA
	case "Expected backup repository is missing":
		results["destination"], results["freshness"], results["evidence"], results["stanza"] = result.Reason("repo_missing"), result.Reason("unknown"), result.NA, result.NA
	case "Invalid successful backup completion timestamp":
		results["evidence"], results["freshness"] = result.Reason("invalid_timestamp"), result.Reason("unknown")
	case "No successful completed backup recorded":
		results["freshness"] = result.Reason("no_success")
	case "Last successful backup exceeds maximum age":
		// Re-derived below from the age.
	default:
		results["freshness"] = result.Reason("unknown")
	}
	// A destination change leaves a barrier until a newer backup completes.
	if !v.entry.NotBefore.IsZero() && (r.LastSuccess == nil || !r.LastSuccess.After(v.entry.NotBefore)) && results["destination"] == result.OK {
		results["destination"] = result.Reason("changed", result.Labeled("since", result.Time(v.entry.NotBefore)))
	}
	item := result.Item{Target: key, Results: results, Values: values}
	if r.LastSuccess != nil && r.AgeSeconds != nil {
		lastSuccess, baseAge, maxAge, evaluated := *r.LastSuccess, *r.AgeSeconds, r.MaxAgeSeconds, v.evaluatedAt
		// As in backrestwatch's Report: the age advances with the monotonic time
		// since evaluation, and a completion in the future of the wall clock
		// (clock rollback) fails closed without touching the ledger.
		item.Refine = func(now time.Time, it *result.Item) {
			elapsed := now.Sub(evaluated).Seconds()
			if elapsed < 0 {
				elapsed = 0
			}
			age := baseAge + elapsed
			it.Values["ageSeconds"] = age
			if it.Results["freshness"] == result.OK && age > float64(maxAge) {
				it.Results["freshness"] = result.Reason("backup_stale", result.Labeled("last", result.Dur(age)), result.Labeled("max", result.Dur(float64(maxAge))))
			}
			if lastSuccess.After(now) {
				it.Results["evidence"], it.Results["freshness"] = result.Reason("invalid_timestamp"), result.Reason("unknown")
			}
		}
		if r.Reason == "Last successful backup exceeds maximum age" {
			results["freshness"] = result.Reason("backup_stale", result.Labeled("last", result.Dur(baseAge)), result.Labeled("max", result.Dur(float64(maxAge))))
		}
	}
	return item
}
