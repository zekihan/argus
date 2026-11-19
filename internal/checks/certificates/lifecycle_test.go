package certificates

import "testing"

func TestPKIProfilesIgnoreUnrelatedCertificateWithRootName(t *testing.T) {
	cfg := pkiConfig()
	sample := pkiHealthy(t)
	unrelated := decode[Certificate](t, `{
		"metadata":{"namespace":"web","name":"selfsigned-ca"},
		"spec":{"issuerRef":{"name":"letsencrypt"}},
		"status":{"conditions":[{"type":"Ready","status":"False"}]}
	}`)
	sample.Certificates = append(sample.Certificates, unrelated)

	if e := Lifecycle(sample.Certificates, monitorNow); !e.Healthy {
		t.Errorf("pki-lifecycle rejected a healthy root: %v", e.Results)
	}
	if healthy, h := pkiVerdict(t, cfg, sample); !healthy {
		_, e := h.Get(t, PKIRoute)
		t.Errorf("pki rejected a healthy root: %v", e.Results)
	}
}
