package cronjobs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
)

var now = time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)

type fakeAPI struct {
	sample Sample
	err    error
}

func (f *fakeAPI) Collect(context.Context) (Sample, error) { return f.sample, f.err }

func decode[T any](t *testing.T, value string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func inventory(namespace, name string, maxAge int) config.CronJobs {
	c := config.CronJobs{Items: []config.CronJob{{Namespace: namespace, Name: name, MaxAgeSeconds: maxAge, MaxRunSeconds: 3600, MaxConsecutiveFailures: 2}}}
	return c
}

func cron(t *testing.T, namespace, name, body string) CronJob {
	c := decode[CronJob](t, body)
	c.Metadata.Namespace, c.Metadata.Name, c.Metadata.UID = namespace, name, "cron-uid"
	return c
}

// paritySuccess runs freshness.py / sync-freshness.py's evaluate cases: the
// parity verdict is schedule, last_success, collection and inventory.
func paritySuccess(t *testing.T, namespace, name string, maxAge int) {
	route := Route + "/" + namespace + "/" + name
	isoformat := func(age int) string {
		return now.Add(-time.Duration(age) * time.Second).Format("2006-01-02T15:04:05-07:00")
	}
	for _, tc := range []struct {
		age  int
		want int
	}{{0, 200}, {maxAge, 200}, {maxAge + 1, 503}, {-1, 503}} {
		c := cron(t, namespace, name, `{"status":{"lastSuccessfulTime":"`+isoformat(tc.age)+`"}}`)
		h := checktest.New(New(inventory(namespace, name, maxAge), &fakeAPI{sample: Sample{CronJobs: []CronJob{c}}}), now).Refresh(t)
		code, e := h.Get(t, route)
		parity := checktest.Parity(e, "schedule", "last_success")
		if (tc.want == 200) != parity || code != tc.want {
			t.Fatal(tc, code, e)
		}
	}
	for _, body := range []string{
		`{}`,
		`{"status":{"lastScheduleTime":"` + isoformat(0) + `"}}`,
		`{"status":{"lastSuccessfulTime":"invalid"}}`,
		`{"status":{"lastSuccessfulTime":"2026-11-06T00:00:00"}}`,
		`{"spec":{"suspend":true},"status":{"lastSuccessfulTime":"` + isoformat(0) + `"}}`,
	} {
		c := cron(t, namespace, name, body)
		h := checktest.New(New(inventory(namespace, name, maxAge), &fakeAPI{sample: Sample{CronJobs: []CronJob{c}}}), now).Refresh(t)
		if code, e := h.Get(t, route); code != 503 || checktest.Parity(e, "schedule", "last_success") {
			t.Fatal(body, code, e)
		}
	}
	// API failure: unhealthy, and no error text in any response or log.
	h := checktest.New(New(inventory(namespace, name, maxAge), &fakeAPI{err: errors.New("secret")}), now).Refresh(t)
	if code, e := h.Get(t, route); code != 503 || e.Results["collection"] != "collection_error" {
		t.Fatal(code, e)
	}
	h.NoLeak(t, "secret")
}

func TestDmarcFreshnessParity(t *testing.T) { paritySuccess(t, "dmarc", "dmarc-converter", 10800) }
func TestMailvaultSyncParity(t *testing.T) {
	paritySuccess(t, "mailvault", "mailvault-mailvault", 25200)
}
func TestSuspendedReasonAndValues(t *testing.T) { suspendedReason(t) }

func suspendedReason(t *testing.T) {
	c := cron(t, "a", "b", `{"spec":{"suspend":true},"status":{"lastSuccessfulTime":"2026-11-05T23:00:00Z"}}`)
	h := checktest.New(New(inventory("a", "b", 10800), &fakeAPI{sample: Sample{CronJobs: []CronJob{c}}}), now).Refresh(t)
	_, e := h.Get(t, Route+"/a/b")
	if e.Results["schedule"] != "suspended" || e.Results["last_success"] != "ok" || e.Values["ageSeconds"] != 3600.0 || e.Values["suspended"] != true {
		t.Fatal(e)
	}
}

func job(t *testing.T, name, created, start, completion, condition string) Job {
	body := `{"metadata":{"namespace":"a","name":"` + name + `","creationTimestamp":"` + created + `","ownerReferences":[{"kind":"CronJob","uid":"cron-uid","controller":true}]},
	"status":{"startTime":"` + start + `","completionTime":"` + completion + `","conditions":[`
	if condition != "" {
		body += `{"type":"` + condition + `","status":"True","message":"password=hunter2"}`
	}
	return decode[Job](t, body+`]}}`)
}

func TestRunHistoryDetailDoesNotAffectParity(t *testing.T) {
	c := cron(t, "a", "b", `{"status":{"lastSuccessfulTime":"2026-11-05T23:00:00Z"}}`)
	healthy := cron(t, "a", "c", `{"status":{"lastSuccessfulTime":"2026-11-05T23:00:00Z"}}`)
	healthy.Metadata.UID = "other"
	cfg := config.CronJobs{Items: []config.CronJob{
		{Namespace: "a", Name: "b", MaxAgeSeconds: 10800, MaxRunSeconds: 600, MaxConsecutiveFailures: 2},
		{Namespace: "a", Name: "c", MaxAgeSeconds: 10800, MaxRunSeconds: 600, MaxConsecutiveFailures: 2},
	}}
	jobs := []Job{
		job(t, "b-5", "2026-11-05T23:50:00Z", "2026-11-05T23:50:00Z", "", ""),                             // running for 10m: active
		job(t, "b-4", "2026-11-05T23:40:00Z", "2026-11-05T23:40:00Z", "", "Failed"),                       // latest finished: failed
		job(t, "b-3", "2026-11-05T23:30:00Z", "2026-11-05T23:30:00Z", "", "Failed"),                       // second failure
		job(t, "b-2", "2026-11-05T23:20:00Z", "2026-11-05T23:20:00Z", "", "Failed"),                       // third failure
		job(t, "b-1", "2026-11-05T22:00:00Z", "2026-11-05T22:00:00Z", "2026-11-05T23:00:00Z", "Complete"), // success
		job(t, "x-1", "2026-11-05T23:55:00Z", "2026-10-05T23:55:00Z", "", ""),                             // another CronJob's stuck Job
	}
	jobs[5].Metadata.Owners[0].UID = "unrelated"
	api := &fakeAPI{sample: Sample{CronJobs: []CronJob{c, healthy}, Jobs: jobs}}
	h := checktest.New(New(cfg, api), now).Refresh(t)
	_, e := h.Get(t, Route+"/a/b")
	if e.Results["last_run"] != "failed" || e.Results["failures"] != "consecutive_failures(3, max 2)" || e.Results["active"] != "ok" ||
		!checktest.Parity(e, "schedule", "last_success") || e.Values["active"] != 1 {
		t.Fatal(e.Results, e.Values)
	}
	// For two CronJobs where only one fails a new detail dimension, the
	// parity dimensions of the aggregate stay healthy.
	_, aggregate := h.Get(t, Route)
	if !checktest.Parity(aggregate, "schedule", "last_success", "inventory") || aggregate.Results["last_run"] != "1 failing: a/b" {
		t.Fatal(aggregate.Results)
	}
	// For two where only one is stale, the parity aggregate pages.
	api.sample.CronJobs[1] = cron(t, "a", "c", `{"status":{"lastSuccessfulTime":"2026-11-05T20:00:00Z"}}`)
	h.Refresh(t)
	if _, aggregate := h.Get(t, Route); checktest.Parity(aggregate, "schedule", "last_success", "inventory") || aggregate.Results["last_success"] != "1 failing: a/c" {
		t.Fatal(aggregate.Results)
	}
	// Overrun and stuck runs.
	api.sample.Jobs = []Job{
		job(t, "b-2", "2026-11-05T22:00:00Z", "2026-11-05T22:00:00Z", "", ""),
		job(t, "b-1", "2026-11-05T21:00:00Z", "2026-11-05T21:00:00Z", "2026-11-05T21:30:00Z", "Complete"),
	}
	h.Refresh(t)
	_, e = h.Get(t, Route+"/a/b")
	if e.Results["last_run"] != "overran(took 30m, max 10m)" || !strings.HasPrefix(e.Results["active"], "stuck(1") || e.Results["failures"] != "ok" {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, "hunter2")
}

func TestMissingCronJobFailsClosed(t *testing.T) {
	h := checktest.New(New(inventory("a", "b", 10800), &fakeAPI{}), now).Refresh(t)
	code, e := h.Get(t, Route+"/a/b")
	if code != 503 || e.Results["schedule"] != "missing" {
		t.Fatal(e)
	}
	if _, aggregate := h.Get(t, Route); aggregate.Results["inventory"] != "missing(1): a/b" {
		t.Fatal(aggregate.Results)
	}
	forbidden := checktest.New(New(inventory("a", "b", 10800), &fakeAPI{err: &kube.Error{Code: kube.CodeForbidden, Status: 403, Path: "/x"}}), now).Refresh(t)
	if code, e := forbidden.Get(t, Route); code != 503 || e.Results["collection"] != "forbidden" {
		t.Fatal(e)
	}
}
