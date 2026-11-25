package pbs

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
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

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

const tokenValue = "argus@pbs!homelab-argus-2026-11:super-secret-value" //nolint:gosec // Synthetic test token, never leaves fixtures.

func testCert(t *testing.T) ([]byte, tls.Certificate, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pbs-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "pbs"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, caTpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(leafDER)
	return caPEM,
		tls.Certificate{
			Certificate: [][]byte{leafDER},
			PrivateKey:  leafKey,
		},
		hex.EncodeToString(sum[:])
}

func tokenFile(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func serve(t *testing.T, leaf tls.Certificate, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// servePBS serves the datastore and groups endpoints, capturing the token.
func servePBS(t *testing.T, leaf tls.Certificate) (*httptest.Server, *string) {
	t.Helper()
	seenValue := ""
	seen := &seenValue
	srv := serve(t, leaf, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.User != nil || r.URL.RawQuery != "" {
			t.Errorf("token leaked into the URL: %s", r.URL.String())
		}
		*seen = r.Header.Get("Authorization")
		if strings.Contains(*seen, tokenValue) && r.URL.Path != "/api2/json/admin/datastore" &&
			!strings.HasSuffix(r.URL.Path, "/groups") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = ioWrite(w, *seen)
			return
		}
		switch r.URL.Path {
		case "/api2/json/admin/datastore":
			_, _ = ioWrite(w, `{"data":[{"store":"truenas"}]}`)
		case "/api2/json/admin/datastore/truenas/groups":
			_, _ = ioWrite(
				w,
				`{"data":[{"backup-type":"vm","backup-id":"100","last-backup":1700000000,"comment":"web"}]}`,
			)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = ioWrite(w, *seen)
		}
	})
	return srv, seen
}

func TestGroupsUsesTheTokenAndNeverLogsIt(t *testing.T) {
	caPEM, leaf, fingerprint := testCert(t)
	logs := captureLogs(t)
	srv, seen := servePBS(t, leaf)
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.PBS{
		URL:         srv.URL,
		TokenFile:   tokenFile(t, tokenValue),
		CAFile:      caFile,
		Fingerprint: fingerprint,
	}
	api, err := NewAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := api.Groups(t.Context())
	if err != nil || len(groups) != 1 || groups[0].Key() != "truenas/vm/100" ||
		groups[0].Guest != "web" ||
		!groups[0].HasLast {
		t.Fatalf("groups = %+v, err = %v", groups, err)
	}
	if *seen != "PBSAPIToken="+tokenValue {
		t.Fatal("authorization header was not the PBS token")
	}
	if strings.Contains(logs.String(), "super-secret-value") ||
		strings.Contains(errText(err), "super-secret-value") {
		t.Fatalf("token was logged: %s", logs.String())
	}
}

func TestRejectedResponsesDoNotEchoTheToken(t *testing.T) {
	_, leaf, fingerprint := testCert(t)
	logs := captureLogs(t)
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect was followed")
	}))
	t.Cleanup(evil.Close)
	srv := serve(t, leaf, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", evil.URL)
		w.WriteHeader(http.StatusFound)
		_, _ = ioWrite(w, r.Header.Get("Authorization"))
	})
	api, err := NewAPI(
		config.PBS{URL: srv.URL, TokenFile: tokenFile(t, tokenValue), Fingerprint: fingerprint},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.Groups(t.Context())
	if err == nil || runner.Code(err) != "http" ||
		strings.Contains(err.Error(), "super-secret-value") ||
		strings.Contains(logs.String(), "super-secret-value") {
		t.Fatalf("error = %v, logs = %s", err, logs.String())
	}
	plain := tokenFile(t, "hunter2")
	api, err = NewAPI(config.PBS{URL: srv.URL, TokenFile: plain, Fingerprint: fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.Groups(t.Context())
	if err == nil || runner.Code(err) != "token_invalid" ||
		strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("invalid token error = %v", err)
	}
}

func TestFingerprintAndCARejectTheWrongPeer(t *testing.T) {
	_, leaf, fingerprint := testCert(t)
	srv := serve(t, leaf, func(w http.ResponseWriter, r *http.Request) {
		_, _ = ioWrite(w, `{"data":[]}`)
	})
	api, err := NewAPI(
		config.PBS{
			URL:         srv.URL,
			TokenFile:   tokenFile(t, tokenValue),
			Fingerprint: strings.Repeat("ab", 32),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = api.Groups(
		t.Context(),
	); err == nil || runner.Code(err) != "tls" ||
		strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("wrong fingerprint = %v", err)
	}
	api, err = NewAPI(
		config.PBS{URL: srv.URL, TokenFile: tokenFile(t, tokenValue), Fingerprint: fingerprint},
	)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := api.Groups(t.Context())
	if err != nil || len(groups) != 0 {
		t.Fatalf("pinned fingerprint = %+v, %v", groups, err)
	}
}

func ioWrite(w http.ResponseWriter, body string) (int, error) {
	return w.Write([]byte(body)) //nolint:gosec // Test handler serving fixture data.
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
