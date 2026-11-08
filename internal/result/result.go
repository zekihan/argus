// Package result defines the response envelope, the ok/n/a/reason-code result
// vocabulary with allowlisted parameters, and the per-dimension aggregate builder.
//
// A result is exactly "ok", "n/a" or a reason code built by Reason. Reason codes
// are fixed identifiers; their only parameters are typed values (durations,
// counts, percentages, timestamps, object names and mount paths). No API
// message, response body, spec field or error string can be turned into one.
package result

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	OK = "ok"
	NA = "n/a"
	// Pending marks a collection that has not completed its first sample.
	Pending = "pending"
)

// Healthy reports whether a result passes: ok and n/a are healthy.
func Healthy(result string) bool { return result == OK || result == NA }

// Param is an allowlisted reason-code parameter. It can only be built by the
// constructors in this package.
type Param struct{ value string }

var code = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Reason builds "code" or "code(p1, p2)". It panics on a malformed code, which
// is always a programming error: codes are literals.
func Reason(name string, params ...Param) string {
	if !code.MatchString(name) || len(name) > 64 {
		panic("invalid reason code " + strconv.Quote(name))
	}
	if len(params) == 0 {
		return name
	}
	values := make([]string, len(params))
	for i, p := range params {
		values[i] = p.value
	}
	return name + "(" + strings.Join(values, ", ") + ")"
}

// Dur formats seconds as 2d0h, 1h30m, 5m or 45s.
func Dur(seconds float64) Param { return Param{Duration(seconds)} }

// Duration formats seconds as 2d0h, 1h30m, 5m or 45s, by absolute value.
func Duration(seconds float64) string {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return "0s"
	}
	s := int64(math.Abs(seconds))
	switch {
	case s >= 86400:
		return strconv.FormatInt(s/86400, 10) + "d" + strconv.FormatInt(s%86400/3600, 10) + "h"
	case s >= 3600:
		return strconv.FormatInt(s/3600, 10) + "h" + strconv.FormatInt(s%3600/60, 10) + "m"
	case s >= 60:
		return strconv.FormatInt(s/60, 10) + "m"
	default:
		return strconv.FormatInt(s, 10) + "s"
	}
}

// Labeled prefixes a parameter with a fixed word, as in "last 2d0h".
func Labeled(label string, p Param) Param {
	if !code.MatchString(label) {
		panic("invalid parameter label " + strconv.Quote(label))
	}
	return Param{label + " " + p.value}
}

// Int formats a count.
func Int(n int64) Param { return Param{strconv.FormatInt(n, 10)} }

// Pct formats a percentage with no decimals.
func Pct(p float64) Param {
	if math.IsNaN(p) || math.IsInf(p, 0) {
		p = 0
	}
	return Param{strconv.FormatFloat(math.Round(p), 'f', 0, 64) + "%"}
}

// Days formats a day count with one decimal.
func Days(d float64) Param {
	if math.IsNaN(d) || math.IsInf(d, 0) {
		d = 0
	}
	return Param{strconv.FormatFloat(d, 'f', 1, 64) + "d"}
}

// Time formats a timestamp as RFC 3339 UTC.
func Time(t time.Time) Param { return Param{t.UTC().Format(time.RFC3339)} }

var nameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._/:@+=-]`)

// Name formats an object name, condition type or mount path. Characters
// outside a conservative set are replaced and the value is capped.
func Name(s string) Param { return Param{SafeName(s)} }

// SafeName sanitises an identifier for use in results and values.
func SafeName(s string) string {
	if len(s) > 253 {
		s = s[:253]
	}
	return nameUnsafe.ReplaceAllString(s, "_")
}

// ProbeText passes through one fixed host-probe field: printable ASCII only,
// capped in length. It is used for nothing else.
func ProbeText(s string, max int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= max {
			break
		}
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteByte(' ')
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return strings.TrimSpace(b.String())
}

// Item is one checked object: a result per dimension plus raw values.
type Item struct {
	Target  string
	Results map[string]string
	Values  map[string]any
	// Refine, when set, re-derives time-dependent results at response time.
	Refine func(now time.Time, item *Item) `json:"-"`
}

// Collection is one route family: /v1/<Route> aggregates /v1/<Route>/<Target>.
type Collection struct {
	Route      string
	Check      string
	Dimensions []string
	Items      []Item
	// Results holds collection-level dimensions such as inventory.
	Results map[string]string
	Values  map[string]any
	// Singleton collections are one item served at /v1/<Route>; their
	// dimensions live in Results and Items is empty.
	Singleton bool
}

// Envelope is the response body for every check route.
type Envelope struct {
	Check      string            `json:"check"`
	Target     string            `json:"target"`
	Healthy    bool              `json:"healthy"`
	ObservedAt *time.Time        `json:"observedAt"`
	Summary    string            `json:"summary"`
	Results    map[string]string `json:"results"`
	Values     map[string]any    `json:"values"`
	Items      map[string]string `json:"items,omitempty"`
}

const summaryLimit = 512

// Summarize returns "ok" or the failing dimensions in order, capped in length.
func Summarize(results map[string]string, order []string) string {
	var parts []string
	seen := map[string]bool{}
	add := func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		if value, ok := results[name]; ok && !Healthy(value) {
			parts = append(parts, name+": "+value)
		}
	}
	for _, name := range order {
		add(name)
	}
	rest := make([]string, 0, len(results))
	for name := range results {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		add(name)
	}
	if len(parts) == 0 {
		return OK
	}
	return capJoin(parts, "; ")
}

func capJoin(parts []string, separator string) string {
	var b strings.Builder
	for i, part := range parts {
		more := ""
		if i < len(parts)-1 {
			more = fmt.Sprintf("%s+%d more", separator, len(parts)-i-1)
		}
		if b.Len()+len(separator)+len(part)+len(more) > summaryLimit && i > 0 {
			fmt.Fprintf(&b, "%s+%d more", separator, len(parts)-i)
			break
		}
		if i > 0 {
			b.WriteString(separator)
		}
		b.WriteString(part)
	}
	return b.String()
}

func allHealthy(results map[string]string) bool {
	for _, value := range results {
		if !Healthy(value) {
			return false
		}
	}
	return true
}

func observed(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func copyValues(values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for k, v := range values {
		out[k] = v
	}
	return out
}

func (c Collection) order() []string {
	return append([]string{"collection", "inventory"}, c.Dimensions...)
}

// Refined returns the item with time-dependent results derived for now.
func (it Item) Refined(now time.Time) Item {
	out := Item{Target: it.Target, Results: make(map[string]string, len(it.Results)+1), Values: copyValues(it.Values)}
	for k, v := range it.Results {
		out.Results[k] = v
	}
	if it.Refine != nil {
		it.Refine(now, &out)
	}
	return out
}

// Find returns the item with the given target.
func (c Collection) Find(target string) (Item, bool) {
	for _, item := range c.Items {
		if item.Target == target {
			return item, true
		}
	}
	return Item{}, false
}

// ItemEnvelope renders one item. status is "ok" or the collection's failure code.
func (c Collection) ItemEnvelope(item Item, status string, observedAt, now time.Time) Envelope {
	item = item.Refined(now)
	item.Results["collection"] = status
	return Envelope{Check: c.Check, Target: item.Target, Healthy: allHealthy(item.Results), ObservedAt: observed(observedAt),
		Summary: Summarize(item.Results, c.order()), Results: item.Results, Values: item.Values}
}

// Aggregate renders the collection route: one result per dimension naming the
// failing items, plus collection and inventory, and each item's verdict.
func (c Collection) Aggregate(status string, observedAt, now time.Time) Envelope {
	results := map[string]string{"collection": status, "inventory": OK}
	if status == Pending {
		results["inventory"] = Pending
	}
	for k, v := range c.Results {
		results[k] = v
	}
	values := copyValues(c.Values)
	if c.Singleton {
		return Envelope{Check: c.Check, Target: "", Healthy: allHealthy(results), ObservedAt: observed(observedAt),
			Summary: Summarize(results, c.order()), Results: results, Values: values}
	}
	failing := map[string][]string{}
	items := make(map[string]string, len(c.Items))
	unhealthy := 0
	for _, raw := range c.Items {
		item := raw.Refined(now)
		for _, dim := range c.Dimensions {
			if value, ok := item.Results[dim]; ok && !Healthy(value) {
				failing[dim] = append(failing[dim], item.Target)
			}
		}
		item.Results["collection"] = status
		items[item.Target] = Summarize(item.Results, c.order())
		if !allHealthy(item.Results) {
			unhealthy++
		}
	}
	if status != Pending {
		for _, dim := range c.Dimensions {
			if _, preset := c.Results[dim]; preset {
				// The collection reported this dimension itself.
				continue
			}
			targets := failing[dim]
			if len(targets) == 0 {
				results[dim] = OK
				continue
			}
			sort.Strings(targets)
			results[dim] = strconv.Itoa(len(targets)) + " failing: " + capJoin(targets, ", ")
		}
	}
	values["items"] = len(c.Items)
	values["unhealthyItems"] = unhealthy
	return Envelope{Check: c.Route, Target: "", Healthy: allHealthy(results) && unhealthy == 0, ObservedAt: observed(observedAt),
		Summary: Summarize(results, c.order()), Results: results, Values: values, Items: items}
}

// Inventory builds the inventory result from missing and unexpected targets.
func Inventory(missing, unexpected []string) string {
	sort.Strings(missing)
	sort.Strings(unexpected)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "missing("+strconv.Itoa(len(missing))+"): "+capJoin(sanitized(missing), ", "))
	}
	if len(unexpected) > 0 {
		parts = append(parts, "unexpected("+strconv.Itoa(len(unexpected))+"): "+capJoin(sanitized(unexpected), ", "))
	}
	if len(parts) == 0 {
		return OK
	}
	return strings.Join(parts, "; ")
}

func sanitized(names []string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = SafeName(name)
	}
	return out
}

// Set records results for the named dimensions. A dimension a check forgot to
// evaluate fails closed as "unknown" rather than passing.
func Set(dimensions []string, results map[string]string) map[string]string {
	out := make(map[string]string, len(dimensions))
	for _, dim := range dimensions {
		if value, ok := results[dim]; ok && value != "" {
			out[dim] = value
		} else {
			out[dim] = "unknown"
		}
	}
	return out
}
