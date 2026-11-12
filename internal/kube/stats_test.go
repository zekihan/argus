package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNodeStatsVerifiedTLSRotatingTokenAndIsolation(t *testing.T) {
	token := "first-synthetic-token"
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/stats/summary" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("unexpected request")
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	tf := filepath.Join(dir, "token")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0600)
	os.WriteFile(tf, []byte(token), 0600)
	c, err := New("https://apiserver.example", tf, ca, "test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	transport := c.HTTP.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "192.0.2.1:10250" {
			t.Errorf("unexpected target %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	// The fixture certificate has 127.0.0.1 as its IP SAN. The production
	// transport leaves ServerName empty and verifies the actual node IP.
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	for range 2 {
		var out any
		if err := c.NodeStats(t.Context(), "192.0.2.1", &out); CodeOf(err) != CodeForbidden {
			t.Fatal(err)
		}
		token = "rotated-synthetic-token"
		os.WriteFile(tf, []byte(token), 0600)
	}
	if requests != 2 || !c.Health(time.Time{}).LastFailure.IsZero() || c.Health(time.Time{}).ForbiddenPaths != 0 {
		t.Fatal("kubelet failure contaminated API health")
	}
	transport.CloseIdleConnections()
	transport.TLSClientConfig = &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
	var out any
	if err := c.NodeStats(t.Context(), "192.0.2.1", &out); CodeOf(err) != CodeUnreachable {
		t.Fatal("untrusted TLS passed", err)
	}
	if requests != 2 {
		t.Fatal("token sent without verified TLS")
	}
}

func TestNodeStatsRejectsUnsafeAddresses(t *testing.T) {
	c := &Client{}
	for _, address := range []string{"", "127.0.0.1", "::1", "0.0.0.0", "224.0.0.1", "example.org/path", "192.0.2.1:443", "169.254.169.254"} {
		if err := c.NodeStats(t.Context(), address, new(any)); CodeOf(err) != CodeInvalid {
			t.Fatal(address, err)
		}
	}
}
