package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/argus/internal/runner"
)

const testToken = "synthetic-monitoring-token-for-local-tests-only"

type fakeRoutes struct{}

func (fakeRoutes) Lookup(path string, _ time.Time) (int, any) {
	if path == "things/ns/a" {
		return 503, result.Envelope{Check: "thing", Target: "ns/a", Summary: "ready: not_ready"}
	}
	return 404, map[string]string{"reason": "unknown route"}
}

func (fakeRoutes) Index(time.Time) (int, runner.Index) {
	return 200, runner.Index{Envelope: result.Envelope{Check: "argus", Healthy: true, Results: map[string]string{"apiserver": "ok"}},
		Routes: map[string]string{"/v1/things/ns/a": "ready: not_ready"}}
}

func writeToken(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAuthorizationOnEveryRouteButHealthz(t *testing.T) {
	path := writeToken(t, testToken+"\n")
	handler := Handler(fakeRoutes{}, path)
	for _, route := range []string{"/v1/checks", "/v1/things", "/v1/things/ns/a", "/v1/unknown", "/unknown", "/v1/"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
			for _, header := range []string{"", "Bearer wrong", "Basic " + testToken} {
				req := httptest.NewRequest(method, route, nil)
				req.Header.Set("Authorization", header)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code != 401 || response.Header().Get("WWW-Authenticate") == "" || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal(route, method, response)
				}
				if method == http.MethodHead && response.Body.Len() != 0 {
					t.Fatal("HEAD leaked body")
				}
				for _, field := range []string{"ns/a", "not_ready", testToken, "apiserver"} {
					if strings.Contains(response.Body.String(), field) {
						t.Fatal("unauthenticated metadata leak", response.Body.String())
					}
				}
			}
		}
	}
	request := func(route, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	for route, code := range map[string]int{"/v1/checks": 200, "/v1/things/ns/a": 503, "/v1/missing": 404, "/v1/": 404, "/v1/things/": 404, "/other": 404, "/healthz": 200} {
		if got := request(route, testToken); got.Code != code {
			t.Fatal(route, got.Code, got.Body.String())
		}
	}
	rotated := testToken + "-rotated"
	if err := os.WriteFile(path, []byte(rotated), 0600); err != nil {
		t.Fatal(err)
	}
	if request("/v1/checks", testToken).Code != 401 || request("/v1/checks", rotated).Code != 200 {
		t.Fatal("rotation did not take effect")
	}
	duplicate := httptest.NewRequest(http.MethodGet, "/v1/checks", nil)
	duplicate.Header.Add("Authorization", "Bearer "+rotated)
	duplicate.Header.Add("Authorization", "Bearer "+rotated)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, duplicate)
	if recorder.Code != 401 {
		t.Fatal("duplicate credentials accepted")
	}
	for _, invalid := range []string{"", "short", strings.Repeat("x", 4097), testToken + "\n\n"} {
		if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if request("/v1/checks", rotated).Code != 503 || request("/healthz", "").Code != 200 {
			t.Fatal("invalid credential did not fail closed or broke probes")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if request("/v1/checks", rotated).Code != 503 || request("/healthz", "").Code != 200 {
		t.Fatal("missing credential did not fail closed")
	}
	for method, code := range map[string]int{"HEAD": 200, "POST": 405} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, "/healthz", nil))
		if recorder.Code != code || (method == "HEAD" && recorder.Body.Len() != 0) {
			t.Fatal(method, recorder.Code)
		}
	}
}

func certificate(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func servedName(t *testing.T, address string) string {
	t.Helper()
	conn, err := tls.Dial("tcp", address, &tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- inspects the served certificate only.
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Subject.CommonName
}

func TestServesTLSAndFollowsCertificateRenewal(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := certificate(t, dir, "first")
	reloader, err := NewReloader(certFile, keyFile, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, func(ctx context.Context) { <-ctx.Done() }, fakeRoutes{}, writeToken(t, testToken), reloader, nil)
	}()
	if name := servedName(t, listener.Addr().String()); name != "first" {
		t.Fatal(name)
	}
	certificate(t, dir, "renewed")
	time.Sleep(20 * time.Millisecond)
	if name := servedName(t, listener.Addr().String()); name != "renewed" {
		t.Fatal("renewal not picked up", name)
	}
	// A broken pair keeps the last good certificate serving.
	if err := os.WriteFile(keyFile, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if name := servedName(t, listener.Addr().String()); name != "renewed" {
		t.Fatal("broken renewal replaced the certificate", name)
	}
	plain, err := http.Get("http://" + listener.Addr().String() + "/healthz")
	if err == nil && plain.StatusCode == 200 {
		t.Fatal("plain HTTP served on the TLS listener")
	}
	if plain != nil {
		plain.Body.Close()
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(17 * time.Second):
		t.Fatal("shutdown hung")
	}
	if _, err := NewReloader(certFile, keyFile, time.Second, nil); err == nil {
		t.Fatal("broken pair accepted at startup")
	}
}

func TestServeBoundsWaitForBlockedPoller(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, listener, func(context.Context) { close(started); <-release }, fakeRoutes{}, "", nil,
			slog.New(slog.NewTextHandler(io.Discard, nil)), 50*time.Millisecond)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("poller did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("serve error = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not return by its shutdown deadline")
	}
}
