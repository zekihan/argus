package certificates

import (
	"time"

	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

// Profile is the PKI lifetime policy: check_postgres_certificates.py's assess
// with the service margin (90 days) or the CLI margin (30 days).
type Profile struct {
	RootNamespace, RootName, IssuerName string
	LeafDays, MarginDays                int
}

var pkiDimensions = []string{"ready", "readiness_generation", "expiry", "ca_key_pinned", "issuer_lifetime", "ca_renewal"}

const day = 24 * time.Hour

// Assess evaluates every certificate under the shared CA issuer, and the
// root itself, exactly as check_postgres_certificates.py's assess does.
// It returns one item per certificate and whether the root CA is present.
func (p Profile) Assess(certificates []Certificate, now time.Time) ([]result.Item, bool) {
	var items []result.Item
	rootPresent := false
	for _, cert := range certificates {
		meta := cert.Metadata
		if meta.Name != p.RootName && cert.Spec.IssuerRef.Name != p.IssuerName {
			continue
		}
		status := cert.status()
		isRoot := meta.Name == p.RootName && meta.Namespace == p.RootNamespace
		isCA := cert.Spec.IsCA
		results := map[string]string{"ca_key_pinned": result.NA, "issuer_lifetime": result.NA, "ca_renewal": result.NA}
		values := map[string]any{"isCA": isCA}
		if isRoot {
			results["ca_key_pinned"] = result.OK
			if cert.Spec.PrivateKey == nil || cert.Spec.PrivateKey.RotationPolicy != "Never" {
				results["ca_key_pinned"] = result.Reason("not_pinned")
			}
		}
		results["ready"] = result.Reason("not_ready")
		for _, c := range status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				results["ready"] = result.OK
			}
		}
		results["readiness_generation"] = result.NA
		if meta.Generation != nil {
			results["readiness_generation"] = result.Reason("stale", result.Labeled("generation", result.Int(*meta.Generation)))
			for _, c := range status.Conditions {
				if c.Type == "Ready" && c.Status == "True" && c.ObservedGeneration != nil && *c.ObservedGeneration == *meta.Generation {
					results["readiness_generation"] = result.OK
				}
			}
		}
		switch {
		case status.NotAfter == nil || *status.NotAfter == "":
			results["expiry"] = result.Reason("unknown")
		default:
			notAfter, err := kube.ParseTime(*status.NotAfter)
			if err != nil {
				results["expiry"] = result.Reason("invalid_timestamp")
				break
			}
			remaining := notAfter.Sub(now)
			values["notAfter"], values["daysRemaining"] = notAfter.Format(time.RFC3339), remaining.Hours()/24
			switch {
			case remaining <= 0:
				results["expiry"] = result.Reason("expired")
			case remaining <= 30*day:
				results["expiry"] = result.Reason("expires_soon", result.Labeled("left", result.Days(remaining.Hours()/24)))
			default:
				results["expiry"] = result.OK
			}
			if isCA {
				need := time.Duration(p.LeafDays+p.MarginDays) * day
				results["issuer_lifetime"] = result.OK
				if remaining < need {
					results["issuer_lifetime"] = result.Reason("insufficient", result.Labeled("left", result.Days(remaining.Hours()/24)),
						result.Labeled("need", result.Days(need.Hours()/24)))
				}
			}
		}
		if isCA {
			switch {
			case status.RenewalTime == nil || *status.RenewalTime == "":
				results["ca_renewal"] = result.Reason("unknown")
			default:
				renewal, err := kube.ParseTime(*status.RenewalTime)
				switch {
				case err != nil:
					results["ca_renewal"] = result.Reason("invalid_timestamp")
				case renewal.Sub(now) <= 30*day:
					results["ca_renewal"] = result.Reason("due", result.Labeled("in", result.Dur(renewal.Sub(now).Seconds())))
				default:
					results["ca_renewal"] = result.OK
				}
				if err == nil {
					values["renewalTime"] = renewal.Format(time.RFC3339)
				}
			}
			if isRoot {
				rootPresent = true
			}
		}
		items = append(items, result.Item{Target: cert.key(), Results: result.Set(pkiDimensions, results), Values: values})
	}
	return items, rootPresent
}

// Lifecycle is the pki-lifecycle CLI profile: the same per-certificate
// assertions with the 30-day margin, and no pin, bundles or inventory, so it
// works from an offline snapshot.
func Lifecycle(certificates []Certificate, now time.Time) result.Envelope {
	profile := Profile{RootNamespace: "cert-manager", RootName: "selfsigned-ca", IssuerName: "selfsigned-ca-issuer", LeafDays: 90, MarginDays: 30}
	items, rootPresent := profile.Assess(certificates, now)
	collection := result.Collection{Route: "pki-lifecycle", Check: "pki-lifecycle/certificate", Dimensions: pkiDimensions, Items: items,
		Results: map[string]string{"inventory": result.NA, "issuer_present": result.OK}}
	if !rootPresent {
		collection.Results["issuer_present"] = result.Reason("issuer_certificate_missing")
	}
	return collection.Aggregate(result.OK, now, now)
}
