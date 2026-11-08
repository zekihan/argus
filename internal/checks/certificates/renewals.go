package certificates

import (
	"context"
	"sort"
	"strconv"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

const Route = "certificates"

// ready, renewal and expiry are certificate_monitor.py's /renewals rules;
// issuer and request are new detail.
var dimensions = []string{"ready", "renewal", "expiry", "issuer", "request"}

type Sample struct {
	Certificates   []Certificate
	Requests       []CertificateRequest
	Issuers        []Issuer
	ClusterIssuers []Issuer
}

type Collector interface {
	Collect(ctx context.Context) (Sample, error)
}

// API lists certificates, CertificateRequests, Issuers and ClusterIssuers
// cluster-wide (CSRs and public certificates only; no Secrets).
type API struct{ Client *kube.Client }

func (a API) Collect(ctx context.Context) (Sample, error) {
	var s Sample
	var err error
	if s.Certificates, err = ListCertificates(ctx, a.Client); err != nil {
		return Sample{}, err
	}
	if s.Requests, err = kube.List[CertificateRequest](ctx, a.Client, "/apis/cert-manager.io/v1/certificaterequests", ""); err != nil {
		return Sample{}, err
	}
	if s.Issuers, err = kube.List[Issuer](ctx, a.Client, "/apis/cert-manager.io/v1/issuers", ""); err != nil {
		return Sample{}, err
	}
	if s.ClusterIssuers, err = kube.List[Issuer](ctx, a.Client, "/apis/cert-manager.io/v1/clusterissuers", ""); err != nil {
		return Sample{}, err
	}
	return s, nil
}

// Renewals reports every certificate not issued by an excluded issuer (the
// shared CA issuer has its own pki profile).
type Renewals struct {
	config config.Certificates
	api    Collector
}

func NewRenewals(c config.Certificates, api Collector) *Renewals {
	return &Renewals{config: c, api: api}
}

func (r *Renewals) Name() string { return "certificates" }

func (r *Renewals) Describe() []result.Collection {
	collection := result.Collection{Route: Route, Check: "certificate", Dimensions: dimensions}
	for _, expected := range r.config.Expected {
		collection.Items = append(collection.Items, result.Item{Target: expected.Key()})
	}
	return []result.Collection{collection}
}

func (r *Renewals) Collect(ctx context.Context, clock func() time.Time) ([]result.Collection, error) {
	sample, err := r.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	// Evaluate after reading: objects updated during the read are not "future".
	now := clock()
	return []result.Collection{r.Evaluate(sample, now)}, nil
}

func (r *Renewals) Evaluate(s Sample, now time.Time) result.Collection {
	excluded := map[string]bool{}
	for _, name := range r.config.ExcludeIssuers {
		excluded[name] = true
	}
	issuers := map[string]Issuer{}
	for _, issuer := range s.Issuers {
		issuers["Issuer/"+issuer.Metadata.Namespace+"/"+issuer.Metadata.Name] = issuer
	}
	for _, issuer := range s.ClusterIssuers {
		issuers["ClusterIssuer//"+issuer.Metadata.Name] = issuer
	}
	collection := result.Collection{Route: Route, Check: "certificate", Dimensions: dimensions}
	present := map[string]bool{}
	expectedKeys := map[string]bool{}
	for _, expected := range r.config.Expected {
		expectedKeys[expected.Key()] = true
	}
	for _, cert := range s.Certificates {
		present[cert.key()] = true
		// An explicitly expected certificate is reported even under an
		// excluded issuer.
		if excluded[cert.Spec.IssuerRef.Name] && !expectedKeys[cert.key()] {
			continue
		}
		collection.Items = append(collection.Items, r.certificate(cert, s.Requests, issuers, now))
	}
	var missing []string
	for _, expected := range r.config.Expected {
		if !present[expected.Key()] {
			missing = append(missing, expected.Key())
			results := map[string]string{"ready": result.Reason("missing")}
			for _, dim := range dimensions[1:] {
				results[dim] = result.NA
			}
			collection.Items = append(collection.Items, result.Item{Target: expected.Key(), Results: results, Values: map[string]any{}})
		}
	}
	sort.Slice(collection.Items, func(i, j int) bool { return collection.Items[i].Target < collection.Items[j].Target })
	collection.Results = map[string]string{"inventory": result.Inventory(missing, nil)}
	return collection
}

func (r *Renewals) certificate(cert Certificate, requests []CertificateRequest, issuers map[string]Issuer, now time.Time) result.Item {
	status := cert.status()
	results := map[string]string{"ready": result.OK, "renewal": result.NA, "expiry": result.NA}
	values := map[string]any{"issuer": result.SafeName(cert.Spec.IssuerRef.Name)}
	if status.Revision != nil {
		values["revision"] = *status.Revision
	}
	// Not ready for longer than the grace period; the transition defaults to
	// the certificate's creation, as in certificate_monitor.py.
	ready, found := kube.FindCondition(status.Conditions, "Ready")
	values["ready"] = found && ready.Status == "True"
	if !found || ready.Status != "True" {
		transition := ready.LastTransitionTime
		if transition == "" {
			transition = cert.Metadata.CreationTimestamp
		}
		since, err := kube.ParseTime(transition)
		switch {
		case err != nil:
			results["ready"] = result.Reason("not_ready")
		case now.Sub(since) > time.Duration(r.config.NotReadySeconds)*time.Second:
			results["ready"] = result.Reason("not_ready", result.Labeled("for", result.Dur(now.Sub(since).Seconds())))
		}
	}
	if status.RenewalTime != nil && *status.RenewalTime != "" {
		renewal, err := kube.ParseTime(*status.RenewalTime)
		switch {
		case err != nil:
			results["renewal"] = result.Reason("invalid_timestamp")
		case now.Sub(renewal) > time.Duration(r.config.RenewalOverdueSeconds)*time.Second:
			results["renewal"] = result.Reason("overdue", result.Labeled("since", result.Dur(now.Sub(renewal).Seconds())))
		default:
			results["renewal"] = result.OK
		}
		if err == nil {
			values["renewalTime"] = renewal.Format(time.RFC3339)
		}
	}
	if status.NotAfter != nil && *status.NotAfter != "" {
		notAfter, err := kube.ParseTime(*status.NotAfter)
		switch {
		case err != nil:
			results["expiry"] = result.Reason("invalid_timestamp")
		case notAfter.Sub(now) <= 0:
			results["expiry"] = result.Reason("expired")
		case notAfter.Sub(now) <= time.Duration(r.config.ExpiryDays)*day:
			results["expiry"] = result.Reason("expires_soon", result.Labeled("left", result.Days(notAfter.Sub(now).Hours()/24)))
		default:
			results["expiry"] = result.OK
		}
		if err == nil {
			values["notAfter"], values["daysRemaining"] = notAfter.Format(time.RFC3339), notAfter.Sub(now).Hours()/24
		}
	}
	results["issuer"] = issuerResult(cert, issuers)
	results["request"] = requestResult(cert, requests)
	return result.Item{Target: cert.key(), Results: result.Set(dimensions, results), Values: values}
}

func issuerResult(cert Certificate, issuers map[string]Issuer) string {
	ref := cert.Spec.IssuerRef
	if ref.Group != "" && ref.Group != "cert-manager.io" {
		// External issuers have their own readiness API.
		return result.NA
	}
	key := "Issuer/" + cert.Metadata.Namespace + "/" + ref.Name
	if ref.Kind == "ClusterIssuer" {
		key = "ClusterIssuer//" + ref.Name
	} else if ref.Kind != "" && ref.Kind != "Issuer" {
		return result.NA
	}
	issuer, ok := issuers[key]
	if !ok {
		return result.Reason("missing", result.Name(ref.Name))
	}
	if !kube.ConditionTrue(issuer.Status.Conditions, "Ready") {
		return result.Reason("not_ready", result.Name(ref.Name))
	}
	return result.OK
}

// requestResult inspects the latest CertificateRequest (highest revision)
// controlled by the certificate.
func requestResult(cert Certificate, requests []CertificateRequest) string {
	var latest *CertificateRequest
	latestRevision := int64(-1)
	for i := range requests {
		request := &requests[i]
		if request.Metadata.Namespace != cert.Metadata.Namespace || !kube.OwnedBy(request.Metadata, "Certificate", cert.Metadata.UID) {
			continue
		}
		revision, err := strconv.ParseInt(request.Metadata.Annotations["cert-manager.io/certificate-revision"], 10, 64)
		if err != nil {
			revision = 0
		}
		if revision > latestRevision {
			latest, latestRevision = request, revision
		}
	}
	if latest == nil {
		return result.NA
	}
	conditions := latest.Status.Conditions
	switch {
	case kube.ConditionTrue(conditions, "Denied"):
		return result.Reason("denied", result.Labeled("revision", result.Int(latestRevision)))
	case kube.ConditionTrue(conditions, "InvalidRequest"):
		return result.Reason("invalid", result.Labeled("revision", result.Int(latestRevision)))
	}
	if ready, ok := kube.FindCondition(conditions, "Ready"); ok && ready.Status == "False" && ready.Reason == "Failed" {
		return result.Reason("failed", result.Labeled("revision", result.Int(latestRevision)))
	}
	return result.OK
}
