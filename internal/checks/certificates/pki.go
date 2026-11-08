package certificates

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
)

const PKIRoute = "pki"

// PKISample is what the service profile reads.
type PKISample struct {
	Certificates []Certificate
	Requests     []CertificateRequest
	Bundles      []Bundle
}

type PKICollector interface {
	Collect(ctx context.Context) (PKISample, error)
}

// PKIAPI lists certificates cluster-wide, CertificateRequests in the root's
// namespace and, only when bundles are expected, trust-manager Bundles.
type PKIAPI struct {
	Client        *kube.Client
	RootNamespace string
	Bundles       bool
}

func (a PKIAPI) Collect(ctx context.Context) (PKISample, error) {
	var s PKISample
	var err error
	if s.Certificates, err = ListCertificates(ctx, a.Client); err != nil {
		return PKISample{}, err
	}
	if s.Requests, err = kube.List[CertificateRequest](ctx, a.Client, "/apis/cert-manager.io/v1/namespaces/"+a.RootNamespace+"/certificaterequests", ""); err != nil {
		return PKISample{}, err
	}
	if a.Bundles {
		if s.Bundles, err = kube.List[Bundle](ctx, a.Client, "/apis/trust.cert-manager.io/v1alpha1/bundles", ""); err != nil {
			return PKISample{}, err
		}
	}
	return s, nil
}

// PKI is certificate_monitor.py's /certificates: the shared CA's lifetime
// with the service's 90-day margin, the expected inventory, the root pin and
// trust-manager bundle publication.
type PKI struct {
	config config.PKI
	api    PKICollector
}

func NewPKI(c config.PKI, api PKICollector) *PKI { return &PKI{config: c, api: api} }

func (p *PKI) Name() string { return "pki" }

func (p *PKI) Describe() []result.Collection {
	return []result.Collection{{Route: PKIRoute, Check: "pki/certificate", Dimensions: pkiDimensions}}
}

func (p *PKI) profile() Profile {
	return Profile{RootNamespace: p.config.RootNamespace, RootName: p.config.RootName, IssuerName: p.config.IssuerName,
		LeafDays: p.config.LeafDays, MarginDays: p.config.MarginDays}
}

func (p *PKI) Collect(ctx context.Context, clock func() time.Time) ([]result.Collection, error) {
	sample, err := p.api.Collect(ctx)
	if err != nil {
		return nil, err
	}
	// Evaluate after reading: objects updated during the read are not "future".
	now := clock()
	return []result.Collection{p.Evaluate(sample, now)}, nil
}

func (p *PKI) Evaluate(s PKISample, now time.Time) result.Collection {
	items, rootPresent := p.profile().Assess(s.Certificates, now)
	collection := result.Collection{Route: PKIRoute, Check: "pki/certificate", Dimensions: pkiDimensions, Items: items, Values: map[string]any{}}
	collection.Results = map[string]string{"issuer_present": result.OK, "root_pin": result.NA}
	if !rootPresent {
		collection.Results["issuer_present"] = result.Reason("issuer_certificate_missing")
	}
	existing := map[string]Certificate{}
	for _, cert := range s.Certificates {
		existing[cert.key()] = cert
	}
	var missing []string
	for _, expected := range p.config.ExpectedCertificates {
		if _, ok := existing[expected.Key()]; !ok {
			missing = append(missing, expected.Key())
		}
	}
	collection.Results["inventory"] = result.Inventory(missing, nil)
	if root, ok := existing[p.config.RootNamespace+"/"+p.config.RootName]; ok {
		collection.Results["root_pin"] = p.rootPin(root, s.Requests, collection.Values)
	}
	collection.Results["bundles"] = p.bundles(s.Bundles)
	return collection
}

// rootPin requires exactly one CertificateRequest for the root's current
// revision, controlled by the root Certificate, whose public certificate has
// the reviewed fingerprint or, after a same-key renewal, the reviewed SPKI.
func (p *PKI) rootPin(root Certificate, requests []CertificateRequest, values map[string]any) string {
	status := root.status()
	var candidates []CertificateRequest
	for _, request := range requests {
		if status.Revision == nil || request.Metadata.Annotations["cert-manager.io/certificate-revision"] != strconv.FormatInt(*status.Revision, 10) {
			continue
		}
		if kube.OwnedBy(request.Metadata, "Certificate", root.Metadata.UID) {
			candidates = append(candidates, request)
		}
	}
	if len(candidates) != 1 || candidates[0].Status.Certificate == "" {
		return result.Reason("current_root_request_missing_or_ambiguous")
	}
	public, err := base64.StdEncoding.Strict().DecodeString(candidates[0].Status.Certificate)
	if err != nil {
		return result.Reason("root_certificate_invalid")
	}
	fingerprint, spki, err := publicIdentity(public)
	if err != nil && fingerprint == "" {
		return result.Reason("root_certificate_invalid")
	}
	values["rootSHA256"] = fingerprint
	// A same-key renewal keeps the reviewed public key and stays trusted.
	if fingerprint != p.config.RootSHA256 && (p.config.RootSPKI == "" || err != nil || spki != p.config.RootSPKI) {
		return result.Reason("changed_review_public_trust")
	}
	return result.OK
}

// publicIdentity returns the SHA-256 of a PEM certificate's DER encoding and
// of its SubjectPublicKeyInfo. The SPKI requires exactly one parseable public
// certificate and no private key material.
func publicIdentity(data []byte) (fingerprint, spki string, err error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", "", fmt.Errorf("not a PEM certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	fingerprint = hex.EncodeToString(sum[:])
	text := string(data)
	if strings.Count(text, "-----BEGIN CERTIFICATE-----") != 1 || strings.Contains(text, "PRIVATE KEY") {
		return fingerprint, "", fmt.Errorf("exactly one public certificate required")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fingerprint, "", fmt.Errorf("invalid certificate")
	}
	key := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return fingerprint, hex.EncodeToString(key[:]), nil
}

// bundles requires each expected trust-manager Bundle to be Synced at its
// current generation.
func (p *PKI) bundles(list []Bundle) string {
	if len(p.config.ExpectedBundles) == 0 {
		return result.NA
	}
	byName := map[string]Bundle{}
	for _, b := range list {
		byName[b.Metadata.Name] = b
	}
	var problems []string
	expected := append([]string(nil), p.config.ExpectedBundles...)
	sort.Strings(expected)
	for _, name := range expected {
		bundle, ok := byName[name]
		if !ok {
			problems = append(problems, result.Reason("missing", result.Name(name)))
			continue
		}
		synced := false
		for _, c := range bundle.Status.Conditions {
			if c.Type == "Synced" && c.Status == "True" && c.ObservedGeneration != nil && bundle.Metadata.Generation != nil && *c.ObservedGeneration == *bundle.Metadata.Generation {
				synced = true
			}
		}
		if !synced {
			problems = append(problems, result.Reason("not_synced", result.Name(name)))
		}
	}
	if len(problems) == 0 {
		return result.OK
	}
	return strings.Join(problems, ", ")
}
