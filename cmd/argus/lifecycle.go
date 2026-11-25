package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/zekihan/argus/internal/checks/certificates"
	"github.com/zekihan/argus/internal/config"
)

// pkiLifecycle runs check_postgres_certificates.py's assertions with the
// 30-day margin, from an offline CertificateList snapshot or from the API
// (typically a loopback kubectl proxy for an explicit context).
// LoadSnapshot reads a CertificateList snapshot within the read budget.
func loadSnapshot(path string) ([]certificates.Certificate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, config.FileError("open snapshot", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 64*1024*1024+1))
	if err != nil || len(data) > 64*1024*1024 {
		return nil, errors.New("snapshot exceeds read budget or cannot be read")
	}
	var list struct {
		Items *[]certificates.Certificate `json:"items"`
	}
	if json.Unmarshal(data, &list) != nil || list.Items == nil {
		return nil, errors.New("snapshot is not a CertificateList")
	}
	return *list.Items, nil
}

// listCertificates reads certificates from the API (loopback proxy).
func listCertificates(api apiFlags) ([]certificates.Certificate, error) {
	client, err := api.client(20 * time.Second)
	if err != nil {
		return nil, err
	}
	defer client.HTTP.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return certificates.ListCertificates(ctx, client)
}

func pkiLifecycle(snapshot string, api apiFlags, stdout io.Writer) error {
	var certs []certificates.Certificate
	var err error
	if snapshot != "" {
		certs, err = loadSnapshot(snapshot)
	} else {
		certs, err = listCertificates(api)
	}
	if err != nil {
		return err
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
