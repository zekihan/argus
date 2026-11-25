package certificates

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

func TestRenewalsAPIFailsClosedBeforeAPartialSample(t *testing.T) {
	const secret = "secret-api-body"
	paths := []string{
		"/apis/cert-manager.io/v1/certificates",
		"/apis/cert-manager.io/v1/certificaterequests",
		"/apis/cert-manager.io/v1/issuers",
		"/apis/cert-manager.io/v1/clusterissuers",
	}
	for i, failAt := range paths {
		t.Run(failAt, func(t *testing.T) {
			var seen []string
			server := httptest.NewServer(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen = append(seen, r.URL.Path)
					if r.URL.Path == failAt {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, secret)
						return
					}
					_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
				}),
			)
			defer server.Close()
			client, err := kube.New(server.URL, "", "", "test", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = (API{Client: client}).Collect(t.Context())
			if err == nil || kube.CodeOf(err) != kube.CodeForbidden ||
				strings.Contains(err.Error(), secret) ||
				len(seen) != i+1 {
				t.Fatalf("error = %v, paths = %v", err, seen)
			}
		})
	}
}

func TestPKIAPIListsBundlesOnlyWhenExpected(t *testing.T) {
	const secret = "secret-api-body"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/bundles") {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, secret)
			return
		}
		_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
	}))
	defer server.Close()
	client, err := kube.New(server.URL, "", "", "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (PKIAPI{Client: client, RootNamespace: "cert-manager"}).Collect(
		t.Context(),
	); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatal(paths)
	}
	_, err = (PKIAPI{Client: client, RootNamespace: "cert-manager", Bundles: true}).Collect(
		t.Context(),
	)
	if err == nil || kube.CodeOf(err) != kube.CodeForbidden ||
		strings.Contains(err.Error(), secret) {
		t.Fatal(err)
	}
}

// assertRenewalIssuerNA verifies a foreign issuer reference reads NA.
func assertRenewalIssuerNA(t *testing.T, group, kind string) {
	t.Helper()
	cert := decode[Certificate](t, siteJSON)
	cert.Spec.IssuerRef.Group, cert.Spec.IssuerRef.Kind = group, kind
	if _, e := checktest.New(NewRenewals(renewalsConfig(), &renewalsAPI{sample: Sample{Certificates: []Certificate{cert}}}), monitorNow).
		Refresh(t).
		Get(t, Route+"/web/site"); e.Results["issuer"] != result.NA {
		t.Fatal(e.Results)
	}
}

// assertBadRequests verifies denied/invalid request evidence leaks nothing.
func assertBadRequests(t *testing.T) {
	t.Helper()
	denied := decode[CertificateRequest](
		t,
		`{"metadata":{"namespace":"web","annotations":{"cert-manager.io/certificate-revision":"not-a-number"},"ownerReferences":[{"kind":"Certificate","uid":"site-uid","controller":true}]},
		"status":{"conditions":[{"type":"Denied","status":"True","message":"secret-denied"}]}}`,
	)
	invalid := decode[CertificateRequest](
		t,
		`{"metadata":{"namespace":"web","annotations":{"cert-manager.io/certificate-revision":"4"},"ownerReferences":[{"kind":"Certificate","uid":"site-uid","controller":true}]},
		"status":{"conditions":[{"type":"InvalidRequest","status":"True","message":"secret-invalid"}]}}`,
	)
	h := checktest.New(NewRenewals(renewalsConfig(), &renewalsAPI{sample: Sample{Certificates: []Certificate{decode[Certificate](t, siteJSON)}, Requests: []CertificateRequest{denied, invalid}}}), monitorNow).
		Refresh(t)
	if _, e := h.Get(t, Route+"/web/site"); e.Results["request"] != "invalid(revision 4)" {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, "secret-denied", "secret-invalid")
}

// assertExpiryTimestamps verifies broken expiry and renewal timestamps.
func assertExpiryTimestamps(t *testing.T) {
	t.Helper()
	expired := decode[Certificate](t, siteJSON)
	past := "2020-01-01T00:00:00Z"
	expired.Status.NotAfter = &past
	expired.Status.RenewalTime = &past
	ready := expired.Status.Conditions[0]
	ready.Status = "False"
	ready.LastTransitionTime = "not-a-time"
	expired.Status.Conditions = []kube.Condition{ready}
	overdue := result.Reason(
		"overdue",
		result.Labeled(
			"since",
			result.Dur(
				monitorNow.Sub(time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)).Seconds(),
			),
		),
	)
	if _, e := checktest.New(NewRenewals(renewalsConfig(), &renewalsAPI{sample: Sample{Certificates: []Certificate{expired}}}), monitorNow).
		Refresh(t).
		Get(t, Route+"/web/site"); e.Results["ready"] != "not_ready" ||
		e.Results["expiry"] != "expired" ||
		e.Results["renewal"] != overdue {
		t.Fatal(e.Results)
	}
	bad := "not-a-time"
	broken := decode[Certificate](t, siteJSON)
	broken.Status.NotAfter, broken.Status.RenewalTime = &bad, &bad
	if _, e := checktest.New(NewRenewals(renewalsConfig(), &renewalsAPI{sample: Sample{Certificates: []Certificate{broken}}}), monitorNow).
		Refresh(t).
		Get(t, Route+"/web/site"); e.Results["expiry"] != "invalid_timestamp" ||
		e.Results["renewal"] != "invalid_timestamp" {
		t.Fatal(e.Results)
	}
}

func TestRenewalAndLifecycleFailClosedOnBadEvidence(t *testing.T) {
	assertRenewalIssuerNA(t, "example.com", "External")
	assertRenewalIssuerNA(t, "", "AWSPCAIssuer")
	assertBadRequests(t)
	assertExpiryTimestamps(t)

	root := decode[Certificate](t, rootJSON)
	request := decode[CertificateRequest](t, requestJSON(publicFixture))
	request.Status.Certificate = "!!!not-base64"
	h := checktest.New(NewPKI(pkiConfig(), &pkiAPI{sample: PKISample{Certificates: []Certificate{root}, Requests: []CertificateRequest{request}}}), monitorNow).
		Refresh(t)
	if _, e := h.Get(t, PKIRoute); e.Results["root_pin"] != "root_certificate_invalid" {
		t.Fatal(e.Results)
	}

	soon := lifecycleNow.Add(10 * 24 * time.Hour).Format(time.RFC3339)
	due := lifecycleNow.Add(10 * 24 * time.Hour).Format(time.RFC3339)
	leaf := lifecycleCert(t, soon, due)
	if healthy, got := itemResult(
		t,
		[]Certificate{leaf},
		"expiry",
	); healthy ||
		got != "expires_soon(left 10.0d)" {
		t.Fatal(got)
	}
	if _, got := itemResult(t, []Certificate{leaf}, "ca_renewal"); got != "due(in 10d0h)" {
		t.Fatal(got)
	}
	dead := lifecycleCert(t, lifecycleNow.Add(-time.Hour).Format(time.RFC3339), "not-a-time")
	if healthy, got := itemResult(t, []Certificate{dead}, "expiry"); healthy || got != "expired" {
		t.Fatal(got)
	}
	if _, got := itemResult(t, []Certificate{dead}, "ca_renewal"); got != "invalid_timestamp" {
		t.Fatal(got)
	}
	untimed := lifecycleCert(t, "", "")
	untimed.Status.NotAfter = nil
	if _, got := itemResult(t, []Certificate{untimed}, "expiry"); got != "unknown" {
		t.Fatal(got)
	}
}
