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

var pkiDimensions = []string{
	dimensionReady,
	"readiness_generation",
	"expiry",
	"ca_key_pinned",
	"issuer_lifetime",
	"ca_renewal",
}

const day = 24 * time.Hour

// conditionTrue is the Kubernetes condition status for true; dimensionReady
// is the certificates collection dimension key for readiness.
const (
	conditionTrue  = "True"
	dimensionReady = "ready"
)

// Assess evaluates every certificate under the shared CA issuer, and the
// root itself, exactly as check_postgres_certificates.py's assess does.
// It returns one item per certificate and whether the root CA is present.
// PinnedResult covers the ca_key_pinned dimension.
func pinnedResult(cert Certificate, isRoot bool) string {
	if !isRoot {
		return result.NA
	}
	if cert.Spec.PrivateKey == nil || cert.Spec.PrivateKey.RotationPolicy != "Never" {
		return result.Reason("not_pinned")
	}
	return result.OK
}

// readyResult covers the ready dimension.
func readyResult(status CertificateStatus) string {
	for _, c := range status.Conditions {
		if c.Type == "Ready" && c.Status == conditionTrue {
			return result.OK
		}
	}
	return result.Reason("not_ready")
}

// readinessGeneration covers the readiness_generation dimension.
func readinessGeneration(meta kube.Metadata, status CertificateStatus) string {
	if meta.Generation == nil {
		return result.NA
	}
	for _, c := range status.Conditions {
		if c.Type == "Ready" && c.Status == conditionTrue && c.ObservedGeneration != nil &&
			*c.ObservedGeneration == *meta.Generation {
			return result.OK
		}
	}
	return result.Reason(
		"stale",
		result.Labeled("generation", result.Int(*meta.Generation)),
	)
}

// caRenewalResult covers the ca_renewal dimension.
func caRenewalResult(status CertificateStatus, now time.Time, values map[string]any) string {
	if status.RenewalTime == nil || *status.RenewalTime == "" {
		return result.Reason("unknown")
	}
	renewal, err := kube.ParseTime(*status.RenewalTime)
	if err != nil {
		return result.Reason("invalid_timestamp")
	}
	values["renewalTime"] = renewal.Format(time.RFC3339)
	if renewal.Sub(now) <= 30*day {
		return result.Reason(
			"due",
			result.Labeled("in", result.Dur(renewal.Sub(now).Seconds())),
		)
	}
	return result.OK
}

func (p Profile) Assess(certificates []Certificate, now time.Time) ([]result.Item, bool) {
	var items []result.Item
	rootPresent := false
	for _, cert := range certificates {
		meta := cert.Metadata
		isRoot := meta.Name == p.RootName && meta.Namespace == p.RootNamespace
		if !isRoot && cert.Spec.IssuerRef.Name != p.IssuerName {
			continue
		}
		item, isRoot := p.assessOne(cert, now)
		rootPresent = rootPresent || isRoot
		items = append(items, item)
	}
	return items, rootPresent
}

// expiryResult covers the expiry and issuer_lifetime dimensions.
func (p Profile) expiryResult(
	status CertificateStatus,
	isCA bool,
	now time.Time,
	values map[string]any,
) (string, string) {
	if status.NotAfter == nil || *status.NotAfter == "" {
		return result.Reason("unknown"), result.NA
	}
	notAfter, err := kube.ParseTime(*status.NotAfter)
	if err != nil {
		return result.Reason("invalid_timestamp"), result.NA
	}
	remaining := notAfter.Sub(now)
	values["notAfter"], values["daysRemaining"] = notAfter.Format(
		time.RFC3339,
	), remaining.Hours()/24
	switch {
	case remaining <= 0:
		return result.Reason("expired"), result.NA
	case remaining <= 30*day:
		return result.Reason(
			"expires_soon",
			result.Labeled("left", result.Days(remaining.Hours()/24)),
		), result.NA
	default:
	}
	if !isCA {
		return result.OK, result.NA
	}
	need := time.Duration(p.LeafDays+p.MarginDays) * day
	if remaining < need {
		return result.OK, result.Reason(
			"insufficient",
			result.Labeled("left", result.Days(remaining.Hours()/24)),
			result.Labeled("need", result.Days(need.Hours()/24)),
		)
	}
	return result.OK, result.OK
}

func (p Profile) assessOne(cert Certificate, now time.Time) (result.Item, bool) {
	status := cert.status()
	isCA := cert.Spec.IsCA
	results := map[string]string{
		"ca_key_pinned":   result.NA,
		"issuer_lifetime": result.NA,
		"ca_renewal":      result.NA,
	}
	values := map[string]any{"isCA": isCA}
	isRoot := cert.Metadata.Name == p.RootName && cert.Metadata.Namespace == p.RootNamespace
	results["ca_key_pinned"] = pinnedResult(cert, isRoot)
	results[dimensionReady] = readyResult(status)
	results["readiness_generation"] = readinessGeneration(cert.Metadata, status)
	results["expiry"], results["issuer_lifetime"] = p.expiryResult(status, isCA, now, values)
	if isCA {
		results["ca_renewal"] = caRenewalResult(status, now, values)
	}
	return result.Item{
		Target:  cert.key(),
		Results: result.Set(pkiDimensions, results),
		Values:  values,
	}, isRoot
}

// Lifecycle is the pki-lifecycle CLI profile: the same per-certificate
// assertions with the 30-day margin, and no pin, bundles or inventory, so it
// works from an offline snapshot.
func Lifecycle(certificates []Certificate, now time.Time) result.Envelope {
	profile := Profile{
		RootNamespace: "cert-manager",
		RootName:      "selfsigned-ca",
		IssuerName:    "selfsigned-ca-issuer",
		LeafDays:      90,
		MarginDays:    30,
	}
	items, rootPresent := profile.Assess(certificates, now)
	collection := result.Collection{
		Route:      "pki-lifecycle",
		Check:      "pki-lifecycle/certificate",
		Dimensions: pkiDimensions,
		Items:      items,
		Results:    map[string]string{"inventory": result.NA, "issuer_present": result.OK},
	}
	if !rootPresent {
		collection.Results["issuer_present"] = result.Reason("issuer_certificate_missing")
	}
	return collection.Aggregate(result.OK, now, now)
}
