package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/zekihan/argus/internal/checks/certificates"
)

// pkiLifecycle runs check_postgres_certificates.py's assertions with the
// 30-day margin, from an offline CertificateList snapshot or from the API
// (typically a loopback kubectl proxy for an explicit context).
func pkiLifecycle(snapshot string, api apiFlags, stdout io.Writer) error {
	var certs []certificates.Certificate
	if snapshot != "" {
		f, err := os.Open(snapshot)
		if err != nil {
			return fmt.Errorf("open snapshot: %w", err)
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, 64*1024*1024+1))
		if err != nil || len(data) > 64*1024*1024 {
			return fmt.Errorf("snapshot exceeds read budget or cannot be read")
		}
		var list struct {
			Items *[]certificates.Certificate `json:"items"`
		}
		if json.Unmarshal(data, &list) != nil || list.Items == nil {
			return fmt.Errorf("snapshot is not a CertificateList")
		}
		certs = *list.Items
	} else {
		client, err := api.client(20 * time.Second)
		if err != nil {
			return err
		}
		defer client.HTTP.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if certs, err = certificates.ListCertificates(ctx, client); err != nil {
			return err
		}
	}
	envelope := certificates.Lifecycle(certs, time.Now())
	if err := printJSON(stdout, envelope); err != nil {
		return err
	}
	if !envelope.Healthy {
		return errUnhealthy
	}
	return nil
}
