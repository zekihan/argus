// Package externalsecrets ports sync_monitor.py: ExternalSecrets,
// ClusterExternalSecrets, SecretStores and ClusterSecretStores. Only fixed
// reason codes are reported; provider condition messages and spec data are
// never decoded.
package externalsecrets

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

// dimensionReady is the ExternalSecret readiness dimension key.
const dimensionReady = "ready"

const (
	ExternalSecretsRoute        = "externalsecrets"
	ClusterExternalSecretsRoute = "clusterexternalsecrets"
	SecretStoresRoute           = "secretstores"
	ClusterSecretStoresRoute    = "clustersecretstores"
)

// ready, synced and refresh are sync_monitor.py's ExternalSecret rules; store
// is new.
var (
	externalSecretDimensions = []string{dimensionReady, "synced", "refresh", "store"}
	readyDimensions          = []string{dimensionReady}
)

type StoreRef struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// ExternalSecret decodes only the fields the check reads: never data or
// templates, and never condition messages.
type ExternalSecret struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		RefreshInterval *string   `json:"refreshInterval"`
		SecretStoreRef  *StoreRef `json:"secretStoreRef"`
	} `json:"spec"`
	Status struct {
		Conditions            []kube.Condition `json:"conditions"`
		ObservedGeneration    *int64           `json:"observedGeneration"`
		SyncedResourceVersion string           `json:"syncedResourceVersion"`
		RefreshTime           *string          `json:"refreshTime"`
	} `json:"status"`
}

// Object is any resource judged by its Ready condition alone.
type Object struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		Conditions []kube.Condition `json:"conditions"`
	} `json:"status"`
}

type Sample struct {
	ExternalSecrets        []ExternalSecret
	ClusterExternalSecrets []Object
	SecretStores           []Object
	ClusterSecretStores    []Object
}

type Collector interface {
	Collect(ctx context.Context) (Sample, error)
}

type API struct{ Client *kube.Client }

func (a API) Collect(ctx context.Context) (Sample, error) {
	const base = "/apis/external-secrets.io/v1/"
	var s Sample
	var err error
	if s.ExternalSecrets, err = kube.List[ExternalSecret](
		ctx,
		a.Client,
		base+"externalsecrets",
		"",
	); err != nil {
		return Sample{}, err
	}
	if s.ClusterSecretStores, err = kube.List[Object](
		ctx,
		a.Client,
		base+"clustersecretstores",
		"",
	); err != nil {
		return Sample{}, err
	}
	if s.SecretStores, err = kube.List[Object](ctx, a.Client, base+"secretstores", ""); err != nil {
		return Sample{}, err
	}
	if s.ClusterExternalSecrets, err = kube.List[Object](
		ctx,
		a.Client,
		base+"clusterexternalsecrets",
		"",
	); err != nil {
		return Sample{}, err
	}
	return s, nil
}

type Check struct {
	config config.ExternalSecrets
	api    Collector
}

func New(c config.ExternalSecrets, api Collector) *Check { return &Check{config: c, api: api} }

func (c *Check) Name() string { return "externalsecrets" }

func namespacedTargets(items []config.NamespacedName) []result.Item {
	out := make([]result.Item, 0, len(items))
	for _, item := range items {
		out = append(out, result.Item{Target: item.Key()})
	}
	return out
}

func clusterTargets(items []string) []result.Item {
	out := make([]result.Item, 0, len(items))
	for _, item := range items {
		out = append(out, result.Item{Target: item})
	}
	return out
}

func (c *Check) Describe() []result.Collection {
	return []result.Collection{
		{
			Route:      ExternalSecretsRoute,
			Check:      "externalsecret",
			Dimensions: externalSecretDimensions,
			Items:      namespacedTargets(c.config.ExpectedExternalSecrets),
		},
		{
			Route:      ClusterExternalSecretsRoute,
			Check:      "clusterexternalsecret",
			Dimensions: readyDimensions,
			Items:      clusterTargets(c.config.ExpectedClusterExternalSecrets),
		},
		{
			Route:      SecretStoresRoute,
			Check:      "secretstore",
			Dimensions: readyDimensions,
			Items:      namespacedTargets(c.config.ExpectedSecretStores),
		},
		{
			Route:      ClusterSecretStoresRoute,
			Check:      "clustersecretstore",
			Dimensions: readyDimensions,
			Items:      clusterTargets(c.config.ExpectedClusterSecretStores),
		},
	}
}

func (c *Check) Collect(ctx context.Context, clock func() time.Time) ([]result.Collection, error) {
	sample, err := c.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	// Evaluate after reading: objects updated during the read are not "future".
	now := clock()
	return c.Evaluate(sample, now), nil
}

// Duration parses a nonnegative refresh interval using ESO's Go duration format.
func Duration(value string) (float64, error) {
	interval, err := time.ParseDuration(value)
	if err != nil || strings.HasPrefix(value, "-") {
		return 0, errors.New("invalid refresh interval")
	}
	return interval.Seconds(), nil
}

func ready(conditions []kube.Condition) string {
	if kube.ConditionTrue(conditions, "Ready") {
		return result.OK
	}
	return result.Reason("not_ready")
}

func (c *Check) Evaluate(s Sample, now time.Time) []result.Collection {
	stores := map[string]bool{}
	clusterStores := map[string]bool{}
	for _, store := range s.SecretStores {
		stores[store.Metadata.Namespace+"/"+store.Metadata.Name] = kube.ConditionTrue(
			store.Status.Conditions,
			"Ready",
		)
	}
	for _, store := range s.ClusterSecretStores {
		clusterStores[store.Metadata.Name] = kube.ConditionTrue(store.Status.Conditions, "Ready")
	}
	externalSecrets := result.Collection{
		Route:      ExternalSecretsRoute,
		Check:      "externalsecret",
		Dimensions: externalSecretDimensions,
	}
	present := map[string]bool{}
	for _, es := range s.ExternalSecrets {
		key := es.Metadata.Namespace + "/" + es.Metadata.Name
		present[key] = true
		results, values := c.externalSecret(es, stores, clusterStores, now)
		externalSecrets.Items = append(
			externalSecrets.Items,
			result.Item{
				Target:  key,
				Results: result.Set(externalSecretDimensions, results),
				Values:  values,
			},
		)
	}
	var missing []string
	for _, expected := range c.config.ExpectedExternalSecrets {
		if !present[expected.Key()] {
			missing = append(missing, expected.Key())
			externalSecrets.Items = append(externalSecrets.Items, result.Item{
				Target: expected.Key(),
				Values: map[string]any{},
				Results: map[string]string{
					dimensionReady: result.Reason("missing"),
					"synced":       result.NA,
					"refresh":      result.NA,
					"store":        result.NA,
				},
			})
		}
	}
	externalSecrets.Results = map[string]string{"inventory": result.Inventory(missing, nil)}
	sortItems(&externalSecrets)
	namespacedStores := make([]string, 0, len(c.config.ExpectedSecretStores))
	for _, item := range c.config.ExpectedSecretStores {
		namespacedStores = append(namespacedStores, item.Key())
	}
	return []result.Collection{
		externalSecrets,
		readyCollection(
			ClusterExternalSecretsRoute,
			"clusterexternalsecret",
			s.ClusterExternalSecrets,
			false,
			c.config.ExpectedClusterExternalSecrets,
		),
		readyCollection(SecretStoresRoute, "secretstore", s.SecretStores, true, namespacedStores),
		readyCollection(
			ClusterSecretStoresRoute,
			"clustersecretstore",
			s.ClusterSecretStores,
			false,
			c.config.ExpectedClusterSecretStores,
		),
	}
}

func sortItems(c *result.Collection) {
	sort.Slice(c.Items, func(i, j int) bool { return c.Items[i].Target < c.Items[j].Target })
}

// readyCollection reports every object's Ready condition, whether or not
// anything references it.
func readyCollection(
	route, check string,
	objects []Object,
	namespaced bool,
	expected []string,
) result.Collection {
	c := result.Collection{Route: route, Check: check, Dimensions: readyDimensions}
	present := map[string]bool{}
	for _, o := range objects {
		key := o.Metadata.Name
		if namespaced {
			key = o.Metadata.Namespace + "/" + key
		}
		present[key] = true
		c.Items = append(
			c.Items,
			result.Item{
				Target:  key,
				Results: map[string]string{dimensionReady: ready(o.Status.Conditions)},
				Values:  map[string]any{},
			},
		)
	}
	var missing []string
	for _, key := range expected {
		if !present[key] {
			missing = append(missing, key)
			c.Items = append(
				c.Items,
				result.Item{
					Target:  key,
					Results: map[string]string{dimensionReady: result.Reason("missing")},
					Values:  map[string]any{},
				},
			)
		}
	}
	c.Results = map[string]string{"inventory": result.Inventory(missing, nil)}
	sortItems(&c)
	return c
}

// refreshResult maps the ExternalSecret refresh status to the refresh result.
func (c *Check) refreshResult(es ExternalSecret, now time.Time, values map[string]any) string {
	interval := "1h"
	if es.Spec.RefreshInterval != nil {
		interval = *es.Spec.RefreshInterval
	}
	seconds, err := Duration(interval)
	switch {
	case err != nil:
		return result.Reason("invalid_refresh_interval")
	case seconds == 0:
		return result.NA
	default:
	}
	values["refreshIntervalSeconds"] = seconds
	maximum := 2*seconds + float64(c.config.RefreshGraceSeconds)
	refreshed, err := kube.ParseTime(deref(es.Status.RefreshTime))
	if err != nil {
		return result.Reason("invalid_refresh_time")
	}
	age := now.Sub(refreshed).Seconds()
	values["refreshAgeSeconds"] = age
	if age < 0 || age > maximum {
		return result.Reason(
			"overdue",
			result.Labeled("last", result.Dur(age)),
			result.Labeled("max", result.Dur(maximum)),
		)
	}
	return result.OK
}

// storeResult maps the secret-store reference to the store result.
func storeResult(
	es ExternalSecret,
	stores, clusterStores map[string]bool,
	values map[string]any,
) string {
	ref := es.Spec.SecretStoreRef
	if ref == nil || ref.Name == "" {
		return result.NA
	}
	values["store"] = result.SafeName(ref.Name)
	var isReady, found bool
	kind := ref.Kind
	if kind == "ClusterSecretStore" {
		isReady, found = clusterStores[ref.Name]
	} else {
		kind = "SecretStore"
		isReady, found = stores[es.Metadata.Namespace+"/"+ref.Name]
	}
	switch {
	case !found:
		return result.Reason("missing", result.Name(kind+"/"+ref.Name))
	case !isReady:
		return result.Reason("not_ready", result.Name(kind+"/"+ref.Name))
	default:
		return result.OK
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (c *Check) externalSecret(
	es ExternalSecret,
	stores, clusterStores map[string]bool,
	now time.Time,
) (map[string]string, map[string]any) {
	results := map[string]string{dimensionReady: ready(es.Status.Conditions)}
	values := map[string]any{}
	// ESO records '<generation>-<annotation hash>' rather than observedGeneration.
	readyCondition, _ := kube.FindCondition(es.Status.Conditions, "Ready")
	observed := readyCondition.ObservedGeneration
	if observed == nil {
		observed = es.Status.ObservedGeneration
	}
	synced, _, _ := strings.Cut(es.Status.SyncedResourceVersion, "-")
	generation := es.Metadata.Generation
	switch {
	case generation == nil:
		results["synced"] = result.Reason("generation_unknown")
	case (observed != nil && *observed != *generation) || synced != strconv.FormatInt(*generation, 10):
		results["synced"] = result.Reason(
			"generation_not_synced",
			result.Labeled("generation", result.Int(*generation)),
		)
	default:
		results["synced"] = result.OK
	}
	if generation != nil {
		values["generation"] = *generation
	}
	results["refresh"] = c.refreshResult(es, now, values)
	results["store"] = storeResult(es, stores, clusterStores, values)
	return results, values
}
