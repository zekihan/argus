// Package certificates ports certificate_monitor.py and
// check_postgres_certificates.py: per-certificate renewal health for every
// certificate, the shared-PKI service profile (pki) and the CLI-only lifecycle
// profile (pki-lifecycle).
package certificates

import (
	"context"

	"github.com/zekihan/argus/internal/kube"
)

type IssuerRef struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Group string `json:"group"`
}

type CertificateStatus struct {
	NotAfter    *string          `json:"notAfter"`
	RenewalTime *string          `json:"renewalTime"`
	Revision    *int64           `json:"revision"`
	Conditions  []kube.Condition `json:"conditions"`
}

type Certificate struct {
	Metadata kube.Metadata `json:"metadata"`
	Spec     struct {
		IsCA       bool      `json:"isCA"`
		IssuerRef  IssuerRef `json:"issuerRef"`
		PrivateKey *struct {
			RotationPolicy string `json:"rotationPolicy"`
		} `json:"privateKey"`
	} `json:"spec"`
	// Status may be null; a null status is not ready and has no lifetime.
	Status *CertificateStatus `json:"status"`
}

func (c Certificate) status() CertificateStatus {
	if c.Status == nil {
		return CertificateStatus{}
	}
	return *c.Status
}

func (c Certificate) key() string { return c.Metadata.Namespace + "/" + c.Metadata.Name }

// CertificateRequest carries only the CSR outcome and the public certificate.
type CertificateRequest struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		Certificate string           `json:"certificate"`
		Conditions  []kube.Condition `json:"conditions"`
	} `json:"status"`
}

type Issuer struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		Conditions []kube.Condition `json:"conditions"`
	} `json:"status"`
}

type Bundle struct {
	Metadata kube.Metadata `json:"metadata"`
	Status   struct {
		Conditions []kube.Condition `json:"conditions"`
	} `json:"status"`
}

const certificatesPath = "/apis/cert-manager.io/v1/certificates"

// ListCertificates lists every Certificate cluster-wide.
func ListCertificates(ctx context.Context, client *kube.Client) ([]Certificate, error) {
	return kube.List[Certificate](ctx, client, certificatesPath, "")
}
