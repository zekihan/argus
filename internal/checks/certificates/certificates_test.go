package certificates

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
)

func decode[T any](t *testing.T, value string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Fixtures from platform/cert-manager/tests/test_certificate_monitor.py.
var monitorNow = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)

const rootJSON = `{"metadata":{"namespace":"cert-manager","name":"selfsigned-ca","uid":"root-uid","generation":2},
"spec":{"isCA":true,"privateKey":{"rotationPolicy":"Never"}},
"status":{"revision":2,"notAfter":"2031-10-22T18:16:07Z","renewalTime":"2030-10-22T18:16:07Z",
"conditions":[{"type":"Ready","status":"True","observedGeneration":2}]}}`

// publicFixture is ssl.DER_cert_to_PEM_cert(b"public-certificate-fixture").
var publicFixture = "-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString([]byte("public-certificate-fixture")) + "\n-----END CERTIFICATE-----\n"

func requestJSON(certificate string) string {
	return `{"metadata":{"namespace":"cert-manager","annotations":{"cert-manager.io/certificate-revision":"2"},
"ownerReferences":[{"kind":"Certificate","uid":"root-uid","controller":true}]},
"status":{"certificate":"` + base64.StdEncoding.EncodeToString([]byte(certificate)) + `"}}`
}

func pkiConfig() config.PKI {
	sum := sha256.Sum256([]byte("public-certificate-fixture"))
	c := config.PKI{RootSHA256: hex.EncodeToString(sum[:]), ExpectedCertificates: []config.NamespacedName{{Namespace: "cert-manager", Name: "selfsigned-ca"}}}
	cfg := config.Config{Checks: config.Checks{PKI: &c}}
	cfg.ApplyDefaults()
	return *cfg.Checks.PKI
}

type pkiAPI struct {
	sample PKISample
	err    error
}

func (p *pkiAPI) Collect(context.Context) (PKISample, error) { return p.sample, p.err }

func pkiHealthy(t *testing.T) PKISample {
	return PKISample{Certificates: []Certificate{decode[Certificate](t, rootJSON)}, Requests: []CertificateRequest{decode[CertificateRequest](t, requestJSON(publicFixture))}}
}

// pkiVerdict is the /certificates parity: every per-certificate and
// collection-level result of the pki aggregate.
func pkiVerdict(t *testing.T, c config.PKI, s PKISample) (bool, *checktest.Harness) {
	t.Helper()
	h := checktest.New(NewPKI(c, &pkiAPI{sample: s}), monitorNow).Refresh(t)
	code, e := h.Get(t, PKIRoute)
	return code == 200 && e.Healthy, h
}

func rootPEM(t *testing.T, key *ecdsa.PrivateKey) string {
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic-ca"},
		NotBefore: monitorNow.Add(-24 * time.Hour), NotAfter: monitorNow.Add(1800 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func spki(t *testing.T, key *ecdsa.PrivateKey) string {
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func TestExpectedRootAndCurrentPublicRequestPass(t *testing.T) {
	if ok, h := pkiVerdict(t, pkiConfig(), pkiHealthy(t)); !ok {
		_, e := h.Get(t, PKIRoute)
		t.Fatal(e)
	}
}

func TestChangedRootIsNotSilentlyTrusted(t *testing.T) {
	c := pkiConfig()
	c.RootSHA256 = strings.Repeat("0", 64)
	ok, h := pkiVerdict(t, c, pkiHealthy(t))
	if _, e := h.Get(t, PKIRoute); ok || e.Results["root_pin"] != "changed_review_public_trust" {
		t.Fatal(e.Results)
	}
}

func TestSameKeyRenewalIsAcceptedOnlyWithThePinnedPublicKey(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	renewed := pkiHealthy(t)
	renewed.Requests[0] = decode[CertificateRequest](t, requestJSON(rootPEM(t, key)))
	if _, got, err := publicIdentity([]byte(rootPEM(t, key))); err != nil || got != spki(t, key) {
		t.Fatal("SPKI digest differs from the DER SubjectPublicKeyInfo", err)
	}
	c := pkiConfig()
	if ok, _ := pkiVerdict(t, c, renewed); ok {
		t.Fatal("renewed root trusted without a reviewed SPKI")
	}
	c.RootSPKI = spki(t, key)
	if ok, _ := pkiVerdict(t, c, renewed); !ok {
		t.Fatal("same-key renewal rejected")
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c.RootSPKI = spki(t, other)
	if ok, h := pkiVerdict(t, c, renewed); ok {
		t.Fatal("different key accepted")
	} else if _, e := h.Get(t, PKIRoute); e.Results["root_pin"] != "changed_review_public_trust" {
		t.Fatal(e.Results)
	}
	// A request carrying a private key is never a public trust anchor.
	keyDER, _ := x509.MarshalECPrivateKey(key)
	withKey := rootPEM(t, key) + string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	renewed.Requests[0] = decode[CertificateRequest](t, requestJSON(withKey))
	c.RootSPKI = spki(t, key)
	if ok, h := pkiVerdict(t, c, renewed); ok {
		t.Fatal("certificate with private key material accepted")
	} else {
		h.NoLeak(t, "PRIVATE KEY", base64.StdEncoding.EncodeToString(keyDER)[:40])
	}
}

func bundle(name string, synced bool, generation, observed int) Bundle {
	status := "False"
	if synced {
		status = "True"
	}
	g, o := int64(generation), int64(observed)
	b := Bundle{}
	b.Metadata.Name, b.Metadata.Generation = name, &g
	b.Status.Conditions = []kube.Condition{{Type: "Synced", Status: status, ObservedGeneration: &o}}
	return b
}

func TestExpectedBundlesMustBeSyncedAtTheirGeneration(t *testing.T) {
	c := pkiConfig()
	c.ExpectedBundles = []string{"postgres-ca", "bridge-ca"}
	s := pkiHealthy(t)
	s.Bundles = []Bundle{bundle("postgres-ca", true, 1, 1), bundle("bridge-ca", true, 1, 1)}
	if ok, _ := pkiVerdict(t, c, s); !ok {
		t.Fatal("synced bundles failed")
	}
	for _, tc := range []struct {
		bundles []Bundle
		want    string
	}{
		{[]Bundle{bundle("postgres-ca", true, 1, 1)}, "missing(bridge-ca)"},
		{[]Bundle{bundle("postgres-ca", true, 1, 1), bundle("bridge-ca", false, 1, 1)}, "not_synced(bridge-ca)"},
		{[]Bundle{bundle("postgres-ca", true, 2, 1), bundle("bridge-ca", true, 1, 1)}, "not_synced(postgres-ca)"},
	} {
		s.Bundles = tc.bundles
		ok, h := pkiVerdict(t, c, s)
		if _, e := h.Get(t, PKIRoute); ok || e.Results["bundles"] != tc.want {
			t.Fatal(e.Results)
		}
	}
}

func TestBundlesAreListedOnlyWhenExpected(t *testing.T) {
	if (PKIAPI{}).Bundles {
		t.Fatal("bundles listed by default")
	}
	if c := pkiConfig(); len(c.ExpectedBundles) != 0 {
		t.Fatal(c)
	}
	if ok, h := pkiVerdict(t, pkiConfig(), pkiHealthy(t)); !ok {
		t.Fatal("no bundles expected, none listed")
	} else if _, e := h.Get(t, PKIRoute); e.Results["bundles"] != "n/a" {
		t.Fatal(e.Results)
	}
}

func TestWrongOwnerRevisionMissingOrAmbiguousRequestFails(t *testing.T) {
	for _, requests := range [][]CertificateRequest{nil, {decode[CertificateRequest](t, requestJSON(publicFixture)), decode[CertificateRequest](t, requestJSON(publicFixture))}} {
		s := pkiHealthy(t)
		s.Requests = requests
		if ok, h := pkiVerdict(t, pkiConfig(), s); ok {
			t.Fatal(len(requests))
		} else if _, e := h.Get(t, PKIRoute); e.Results["root_pin"] != "current_root_request_missing_or_ambiguous" {
			t.Fatal(e.Results)
		}
	}
	for _, edit := range []func(*CertificateRequest){
		func(r *CertificateRequest) { r.Metadata.Owners[0].UID = "other" },
		func(r *CertificateRequest) { r.Metadata.Owners[0].Controller = false },
		func(r *CertificateRequest) { r.Metadata.Annotations["cert-manager.io/certificate-revision"] = "1" },
	} {
		s := pkiHealthy(t)
		edit(&s.Requests[0])
		if ok, _ := pkiVerdict(t, pkiConfig(), s); ok {
			t.Fatal("request accepted")
		}
	}
}

func TestInventoryAndLifetimeAndKeyPinFailClosed(t *testing.T) {
	s := pkiHealthy(t)
	s.Certificates = nil
	if ok, h := pkiVerdict(t, pkiConfig(), s); ok {
		t.Fatal("missing root accepted")
	} else if _, e := h.Get(t, PKIRoute); e.Results["issuer_present"] != "issuer_certificate_missing" || e.Results["inventory"] != "missing(1): cert-manager/selfsigned-ca" {
		t.Fatal(e.Results)
	}
	for edit, dim := range map[string]string{"expiry": "issuer_lifetime", "pin": "ca_key_pinned", "generation": "readiness_generation"} {
		root := decode[Certificate](t, rootJSON)
		switch edit {
		case "expiry":
			v := "2027-03-01T00:00:00Z"
			root.Status.NotAfter = &v
		case "pin":
			root.Spec.PrivateKey.RotationPolicy = "Always"
		case "generation":
			g := int64(3)
			root.Metadata.Generation = &g
		}
		s := pkiHealthy(t)
		s.Certificates = []Certificate{root}
		if ok, h := pkiVerdict(t, pkiConfig(), s); ok {
			t.Fatal(edit)
		} else if _, e := h.Get(t, PKIRoute+"/cert-manager/selfsigned-ca"); e.Results[dim] == "ok" {
			t.Fatal(edit, e.Results)
		}
	}
}

func TestAPIErrorDoesNotExposeResponseOrKeepCachedGreen(t *testing.T) {
	api := &pkiAPI{err: errors.New("sensitive body")}
	h := checktest.New(NewPKI(pkiConfig(), api), monitorNow).Refresh(t)
	if code, _ := h.Get(t, PKIRoute); code != 503 {
		t.Fatal(code)
	}
	h.NoLeak(t, "sensitive body")
	api.err, api.sample = nil, pkiHealthy(t)
	h.Refresh(t)
	if code, _ := h.Get(t, PKIRoute); code != 200 {
		t.Fatal(code)
	}
	api.err = errors.New("sensitive body")
	h.Refresh(t)
	if code, _ := h.Get(t, PKIRoute); code != 503 {
		t.Fatal("cached green kept after a failed sample")
	}
}

// Renewals: certificate_monitor.py's /renewals.

type renewalsAPI struct {
	sample Sample
	err    error
}

func (r *renewalsAPI) Collect(context.Context) (Sample, error) { return r.sample, r.err }

func renewalsConfig() config.Certificates {
	c := config.Certificates{ExcludeIssuers: []string{"selfsigned-ca-issuer"}}
	cfg := config.Config{Checks: config.Checks{Certificates: &c}}
	cfg.ApplyDefaults()
	return *cfg.Checks.Certificates
}

const siteJSON = `{"metadata":{"namespace":"web","name":"site","uid":"site-uid"},"spec":{"issuerRef":{"name":"letsencrypt"}},
"status":{"conditions":[{"type":"Ready","status":"True"}],"renewalTime":"2026-11-01T00:00:00Z","notAfter":"2026-12-01T00:00:00Z"}}`

// renewal returns the /renewals parity verdict as an HTTP-like code: 200 when
// ready, renewal, expiry, collection and inventory are healthy.
func renewal(t *testing.T, certs ...Certificate) (int, map[string]string, *checktest.Harness) {
	t.Helper()
	root := decode[Certificate](t, rootJSON)
	root.Spec.IssuerRef.Name = "selfsigned-ca-issuer"
	h := checktest.New(NewRenewals(renewalsConfig(), &renewalsAPI{sample: Sample{Certificates: append([]Certificate{root}, certs...)}}), monitorNow).Refresh(t)
	_, e := h.Get(t, Route)
	if checktest.Parity(e, "ready", "renewal", "expiry", "inventory") {
		return 200, e.Results, h
	}
	return 503, e.Results, h
}

func TestRenewalEndpoint(t *testing.T) {
	for _, issuer := range []string{"letsencrypt", "mtls-clients-v1"} {
		cert := decode[Certificate](t, siteJSON)
		cert.Spec.IssuerRef.Name = issuer
		cert.Status.Conditions = []kube.Condition{{Type: "Ready", Status: "False", LastTransitionTime: "2026-10-22T22:59:59Z"}}
		code, results, h := renewal(t, cert)
		_, item := h.Get(t, Route+"/web/site")
		if code != 503 || results["ready"] != "1 failing: web/site" || !strings.HasPrefix(item.Results["ready"], "not_ready(") {
			t.Fatal(issuer, results, item.Results)
		}
	}
	cert := decode[Certificate](t, siteJSON)
	cert.Status.Conditions = []kube.Condition{{Type: "Ready", Status: "False", LastTransitionTime: "2026-10-22T23:30:00Z"}}
	if code, results, _ := renewal(t, cert); code != 200 {
		t.Fatal("not ready within the grace period paged", results)
	}
	cert = decode[Certificate](t, siteJSON)
	overdue := "2026-10-21T23:59:59Z"
	cert.Status.RenewalTime = &overdue
	if code, results, _ := renewal(t, cert); code != 503 || results["renewal"] != "1 failing: web/site" {
		t.Fatal(results)
	}
	cert = decode[Certificate](t, siteJSON)
	soon := "2026-11-05T00:00:00Z"
	cert.Status.NotAfter = &soon
	if code, results, _ := renewal(t, cert); code != 503 || results["expiry"] != "1 failing: web/site" {
		t.Fatal(results)
	}
	// The shared CA issuer belongs to the pki profile.
	cert.Spec.IssuerRef.Name = "selfsigned-ca-issuer"
	if code, results, _ := renewal(t, cert); code != 200 {
		t.Fatal(results)
	}
}

func TestInvalidRenewalDataFailsOnlyThatCertificate(t *testing.T) {
	cert := decode[Certificate](t, `{"metadata":{"namespace":"web","name":"site"},"spec":{"issuerRef":{"name":"letsencrypt"}},"status":null}`)
	if code, results, _ := renewal(t, cert); code != 503 || results["ready"] != "1 failing: web/site" {
		t.Fatal(results)
	}
	// The pki profile is evaluated separately and stays healthy.
	if ok, _ := pkiVerdict(t, pkiConfig(), PKISample{Certificates: append(pkiHealthy(t).Certificates, cert), Requests: pkiHealthy(t).Requests}); !ok {
		t.Fatal("unrelated certificate failed the pki profile")
	}
}

func TestIssuerAndRequestDetail(t *testing.T) {
	cert := decode[Certificate](t, siteJSON)
	cert.Spec.IssuerRef.Kind = "ClusterIssuer"
	issuer := decode[Issuer](t, `{"metadata":{"name":"letsencrypt"},"status":{"conditions":[{"type":"Ready","status":"False","message":"acme account key=secret-acme"}]}}`)
	failed := decode[CertificateRequest](t, `{"metadata":{"namespace":"web","annotations":{"cert-manager.io/certificate-revision":"3"},"ownerReferences":[{"kind":"Certificate","uid":"site-uid","controller":true}]},
	"status":{"conditions":[{"type":"Ready","status":"False","reason":"Failed","message":"order failed: secret-acme"}]}}`)
	older := decode[CertificateRequest](t, `{"metadata":{"namespace":"web","annotations":{"cert-manager.io/certificate-revision":"2"},"ownerReferences":[{"kind":"Certificate","uid":"site-uid","controller":true}]},
	"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	api := &renewalsAPI{sample: Sample{Certificates: []Certificate{cert}, ClusterIssuers: []Issuer{issuer}, Requests: []CertificateRequest{older, failed}}}
	h := checktest.New(NewRenewals(renewalsConfig(), api), monitorNow).Refresh(t)
	_, e := h.Get(t, Route+"/web/site")
	if e.Results["issuer"] != "not_ready(letsencrypt)" || e.Results["request"] != "failed(revision 3)" || !checktest.Parity(e, "ready", "renewal", "expiry") {
		t.Fatal(e.Results)
	}
	h.NoLeak(t, "secret-acme")
	api.sample.ClusterIssuers = nil
	h.Refresh(t)
	if _, e := h.Get(t, Route+"/web/site"); e.Results["issuer"] != "missing(letsencrypt)" {
		t.Fatal(e.Results)
	}
}

func TestExpectedCertificateMissing(t *testing.T) {
	c := renewalsConfig()
	c.Expected = []config.NamespacedName{{Namespace: "web", Name: "gone"}}
	h := checktest.New(NewRenewals(c, &renewalsAPI{}), monitorNow).Refresh(t)
	if code, e := h.Get(t, Route+"/web/gone"); code != 503 || e.Results["ready"] != "missing" {
		t.Fatal(e)
	}
	if _, e := h.Get(t, Route); e.Results["inventory"] != "missing(1): web/gone" {
		t.Fatal(e.Results)
	}
}

// pki-lifecycle: tests/gitops/test_gitops_postgres_certificates.py.

var lifecycleNow = time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)

func lifecycleCert(t *testing.T, expiry, renewal string) Certificate {
	return decode[Certificate](t, `{"metadata":{"name":"selfsigned-ca","namespace":"cert-manager"},"spec":{"isCA":true,"privateKey":{"rotationPolicy":"Never"}},
	"status":{"notAfter":"`+expiry+`","renewalTime":"`+renewal+`","conditions":[{"type":"Ready","status":"True"}]}}`)
}

func defaultLifecycleCert(t *testing.T) Certificate {
	return lifecycleCert(t, "2031-10-22T00:00:00Z", "2030-10-22T00:00:00Z")
}

func itemResult(t *testing.T, certs []Certificate, dimension string) (bool, string) {
	t.Helper()
	e := Lifecycle(certs, lifecycleNow)
	items, _ := Profile{RootNamespace: "cert-manager", RootName: "selfsigned-ca", IssuerName: "selfsigned-ca-issuer", LeafDays: 90, MarginDays: 30}.Assess(certs, lifecycleNow)
	if len(items) == 0 {
		return e.Healthy, ""
	}
	return e.Healthy, items[0].Results[dimension]
}

func TestLifecycleProfile(t *testing.T) {
	c := defaultLifecycleCert(t)
	c.Spec.PrivateKey = nil
	if healthy, got := itemResult(t, []Certificate{c}, "ca_key_pinned"); healthy || got != "not_pinned" {
		t.Fatal("unpinned root", got)
	}
	c = defaultLifecycleCert(t)
	g, one, two := int64(2), int64(1), int64(2)
	c.Metadata.Generation = &g
	c.Status.Conditions = []kube.Condition{{Type: "Ready", Status: "True", ObservedGeneration: &one}, {Type: "Ready", Status: "False", ObservedGeneration: &two}}
	if healthy, got := itemResult(t, []Certificate{c}, "readiness_generation"); healthy || got != "stale(generation 2)" {
		t.Fatal("stale readiness generation", got)
	}
	other := defaultLifecycleCert(t)
	other.Metadata.Name, other.Spec.IssuerRef.Name = "unrelated-intermediate", "selfsigned-ca-issuer"
	if e := Lifecycle([]Certificate{other}, lifecycleNow); e.Healthy || e.Results["issuer_present"] != "issuer_certificate_missing" {
		t.Fatal("another CA hid the missing root", e.Results)
	}
	if healthy, got := itemResult(t, []Certificate{lifecycleCert(t, "2031-10-22T00:00:00", "2030-10-22T00:00:00Z")}, "expiry"); healthy || got != "invalid_timestamp" {
		t.Fatal("naive timestamp accepted", got)
	}
	if e := Lifecycle([]Certificate{defaultLifecycleCert(t)}, lifecycleNow); !e.Healthy {
		t.Fatal("ready CA with headroom failed", e)
	}
	if healthy, got := itemResult(t, []Certificate{lifecycleCert(t, "2027-01-03T15:08:37Z", "2026-12-04T15:08:37Z")}, "issuer_lifetime"); healthy || !strings.HasPrefix(got, "insufficient(") {
		t.Fatal("short CA accepted", got)
	}
	if healthy, got := itemResult(t, []Certificate{lifecycleCert(t, "2031-10-22T00:00:00Z", "2026-11-01T00:00:00Z")}, "ca_renewal"); healthy || !strings.HasPrefix(got, "due(") {
		t.Fatal("renewal gate", got)
	}
	if e := Lifecycle(nil, lifecycleNow); e.Healthy {
		t.Fatal("empty snapshot passed")
	}
	c = defaultLifecycleCert(t)
	c.Status = &CertificateStatus{}
	if healthy, got := itemResult(t, []Certificate{c}, "expiry"); healthy || got != "unknown" {
		t.Fatal("missing status passed", got)
	}
	acme := decode[Certificate](t, `{"metadata":{"name":"public-http","namespace":"web"},"spec":{"issuerRef":{"name":"letsencrypt"}},"status":{}}`)
	if e := Lifecycle([]Certificate{defaultLifecycleCert(t), acme}, lifecycleNow); !e.Healthy {
		t.Fatal("unrelated ACME certificate failed the database gate", e)
	}
	// The service profile's 90-day margin needs 180 days of root headroom; the
	// CLI's 30-day margin needs 120.
	cert := lifecycleCert(t, "2027-04-21T00:00:00Z", "2027-01-01T00:00:00Z") // 181 days left
	if !Lifecycle([]Certificate{cert}, lifecycleNow).Healthy {
		t.Fatal("CLI profile rejected 181 days")
	}
	cert = lifecycleCert(t, "2027-03-22T00:00:00Z", "2027-01-01T00:00:00Z") // 151 days left
	service, _ := Profile{RootNamespace: "cert-manager", RootName: "selfsigned-ca", IssuerName: "selfsigned-ca-issuer", LeafDays: 90, MarginDays: 90}.Assess([]Certificate{cert}, lifecycleNow)
	if !Lifecycle([]Certificate{cert}, lifecycleNow).Healthy || service[0].Results["issuer_lifetime"] == "ok" {
		t.Fatal("margins not distinct", service[0].Results)
	}
}

func TestExpectedCertificateUnderAnExcludedIssuerIsReported(t *testing.T) {
	c := renewalsConfig()
	c.Expected = []config.NamespacedName{{Namespace: "cert-manager", Name: "selfsigned-ca"}}
	root := decode[Certificate](t, rootJSON)
	root.Spec.IssuerRef.Name = "selfsigned-ca-issuer"
	root.Status.Conditions[0].Status = "False"
	root.Status.Conditions[0].LastTransitionTime = "2026-10-01T00:00:00Z"
	h := checktest.New(NewRenewals(c, &renewalsAPI{sample: Sample{Certificates: []Certificate{root}}}), monitorNow).Refresh(t)
	if code, e := h.Get(t, Route+"/cert-manager/selfsigned-ca"); code != 503 || !strings.HasPrefix(e.Results["ready"], "not_ready") {
		t.Fatal("expected certificate hidden by issuer exclusion", code, e.Results)
	}
}

// certificate_monitor.py rejects anything but exactly one PEM certificate,
// even when the first block has the pinned fingerprint.
func TestRootPinRequiresExactlyOneCertificate(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for name, public := range map[string]string{
		"bundle":       publicFixture + rootPEM(t, key),
		"leading text": "subject=CN example\n" + publicFixture,
		"trailing key": publicFixture + "-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n",
	} {
		s := pkiHealthy(t)
		s.Requests[0] = decode[CertificateRequest](t, requestJSON(public))
		ok, h := pkiVerdict(t, pkiConfig(), s)
		if _, e := h.Get(t, PKIRoute); ok || e.Results["root_pin"] != "root_certificate_invalid" {
			t.Fatal(name, e.Results)
		}
	}
}
