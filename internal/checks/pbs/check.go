package pbs

import (
	"context"
	"sort"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/go-commons/metrics"
)

const (
	Route = "pbs/backups"
	// lastBackup is one series per backup group. A group that disappears keeps
	// its last sample until the process restarts, so a removed backup stays
	// armed. Excluded groups are never recorded.
	lastBackup = "argus_pbs_last_backup_timestamp_seconds"
)

var dimensions = []string{"freshness"}

// Reader lists backup groups.
type Reader interface {
	Groups(ctx context.Context) ([]Group, error)
}

// Check evaluates PBS backup freshness.
type Check struct {
	config config.PBS
	api    Reader
	reg    *metrics.Registry
}

// New returns a PBS backup check. reg may be nil when metrics are disabled.
func New(c config.PBS, api Reader, reg *metrics.Registry) *Check {
	return &Check{config: c, api: api, reg: reg}
}

func (c *Check) Name() string { return "pbs" }

func (c *Check) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: Route, Dimensions: dimensions}
	for _, key := range sorted(c.config.Expected) {
		collection.Items = append(collection.Items, result.Item{Target: key})
	}
	return []result.Collection{collection}
}

func (c *Check) Collect(ctx context.Context, clock func() time.Time) ([]result.Collection, error) {
	groups, err := c.api.Groups(ctx)
	if err != nil {
		return nil, err
	}
	return c.Evaluate(groups, clock()), nil
}

// Evaluate applies freshness, the expected inventory and the exclusion list.
func (c *Check) Evaluate(groups []Group, now time.Time) []result.Collection {
	excluded := map[string]bool{}
	for _, key := range c.config.Exclude {
		excluded[key] = true
	}
	expected := map[string]bool{}
	for _, key := range c.config.Expected {
		expected[key] = true
	}
	seen := map[string]Group{}
	var unexpected []string
	for _, group := range groups {
		key := group.Key()
		if excluded[key] {
			continue
		}
		if _, ok := seen[key]; ok {
			seen[key] = Group{}
			continue
		}
		seen[key] = group
		if len(expected) > 0 && !expected[key] {
			unexpected = append(unexpected, key)
		}
	}
	var missing []string
	keys := map[string]bool{}
	for key := range seen {
		keys[key] = true
	}
	for _, key := range c.config.Expected {
		if !keys[key] {
			missing = append(missing, key)
		}
		keys[key] = true
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	collection := result.Collection{Route: Route, Check: Route, Dimensions: dimensions,
		Results: map[string]string{"inventory": result.Inventory(missing, unexpected)}}
	for _, key := range ordered {
		group, ok := seen[key]
		item := result.Item{Target: key, Values: map[string]any{}}
		switch {
		case !ok:
			item.Results = map[string]string{"freshness": result.Reason("missing")}
		case group.Store == "" && group.Type == "" && group.ID == "":
			item.Results = map[string]string{"freshness": result.Reason("ambiguous")}
		default:
			maximum, single := c.config.MaxAge(key)
			if !single {
				item.Results = map[string]string{"freshness": result.Reason("ambiguous")}
			} else {
				item.Results, item.Values = freshness(group, now, time.Duration(maximum)*time.Second)
			}
			c.observe(group)
		}
		collection.Items = append(collection.Items, item)
	}
	return []result.Collection{collection}
}

func freshness(group Group, now time.Time, maximum time.Duration) (map[string]string, map[string]any) {
	values := map[string]any{}
	if config.PBSName(group.Guest) {
		values["guest"] = group.Guest
	}
	if !group.HasLast {
		return map[string]string{"freshness": result.Reason("no_backup")}, values
	}
	age := now.Sub(group.Last)
	values["lastBackupAt"] = group.Last.UTC().Format(time.RFC3339)
	values["lastBackupAgeSeconds"] = age.Seconds()
	switch {
	case age < 0:
		return map[string]string{"freshness": result.Reason("backup_in_future", result.Dur(age.Seconds()))}, values
	case age > maximum:
		return map[string]string{"freshness": result.Reason("backup_stale", result.Labeled("last", result.Dur(age.Seconds())), result.Labeled("max", result.Dur(maximum.Seconds())))}, values
	default:
		return map[string]string{"freshness": result.OK}, values
	}
}

func (c *Check) observe(group Group) {
	if c.reg == nil || !group.HasLast {
		return
	}
	c.reg.Gauge(lastBackup, metrics.Labels{
		"datastore":   group.Store,
		"backup_type": group.Type,
		"backup_id":   group.ID,
	}).Set(float64(group.Last.Unix()))
}

func sorted(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.Strings(out)
	return out
}
