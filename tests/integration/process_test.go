//go:build integration

package integration

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	monitoringToken = "synthetic-process-monitoring-token-only"
	// Credential-shaped strings planted in condition messages, error bodies,
	// pod environment values and spec fields. None may reach a response or log.
	planted = "PLANTED-SECRET-VALUE-7f3a"
)

func binary(t *testing.T) string {
	t.Helper()
	path := os.Getenv("ARGUS_BINARY")
	if path == "" {
		t.Fatal("run make test/integration")
	}
	return path
}

// fakeAPI is a Kubernetes API with just enough objects for several checks.
type fakeAPI struct {
	mu        sync.Mutex
	ledger    string
	version   int
	puts      int
	forbidden map[string]bool
	down      bool
}

func now() time.Time { return time.Now().UTC() }

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"` + planted + `"}`))
		return
	}
	for prefix := range f.forbidden {
		if strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden: token ` + planted + `"}`))
			return
		}
	}
	list := func(items ...any) {
		if items == nil {
			items = []any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"resourceVersion": "1"}, "items": items})
	}
	created := now().Add(-24 * time.Hour).Format(time.RFC3339)
	recent := now().Add(-time.Hour).Format(time.RFC3339)
	switch r.URL.Path {
	case "/api":
		_, _ = w.Write([]byte(`{"kind":"APIVersions","versions":["v1"]}`))
	case "/api/v1/namespaces/argus/configmaps/argus-state":
		if r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			var object struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
				Data map[string]string `json:"data"`
			}
			if json.Unmarshal(body, &object) != nil || object.Metadata.ResourceVersion != itoa(f.version) {
				w.WriteHeader(http.StatusConflict)
				return
			}
			f.ledger, f.puts = object.Data["pgbackrest.json"], f.puts+1
			f.version++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "argus-state", "namespace": "argus", "resourceVersion": itoa(f.version)},
			"data":     map[string]string{"pgbackrest.json": f.ledger}})
	case "/apis/postgres-operator.crunchydata.com/v1beta1/postgresclusters":
		list(map[string]any{
			"metadata": map[string]any{"namespace": "db", "name": "db", "uid": "cluster-uid", "creationTimestamp": created},
			"spec": map[string]any{"backups": map[string]any{"pgbackrest": map[string]any{
				"repos":  []any{map[string]any{"name": "repo1", "s3": map[string]string{"bucket": "b", "endpoint": "s3.example.com", "region": "r"}}},
				"global": map[string]string{"repo1-path": "/pgbackrest/db/repo1", "repo1-s3-key-secret": planted}}}},
			"status": map[string]any{"pgbackrest": map[string]any{
				"repos":            []any{map[string]any{"name": "repo1", "stanzaCreated": true}},
				"scheduledBackups": []any{map[string]any{"repo": "repo1", "succeeded": 1, "completionTime": recent}}},
				"conditions": []any{map[string]string{"type": "ProxyAvailable", "status": "True", "message": planted}}},
		})
	case "/apis/batch/v1/namespaces/db/jobs":
		list(map[string]any{
			"metadata": map[string]any{"namespace": "db", "name": "backup", "uid": "job", "creationTimestamp": created,
				"labels": map[string]string{"postgres-operator.crunchydata.com/cluster": "db", "postgres-operator.crunchydata.com/pgbackrest-repo": "repo1", "postgres-operator.crunchydata.com/pgbackrest-backup": "manual"}},
			"spec":   map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"env": []any{map[string]string{"name": "PASSWORD", "value": planted}}}}}}},
			"status": map[string]any{"succeeded": 1, "completionTime": created, "conditions": []any{map[string]string{"type": "Failed", "status": "False", "message": planted}}},
		})
	case "/apis/batch/v1/namespaces/db/cronjobs", "/apis/batch/v1/namespaces/app/jobs":
		list()
	case "/apis/batch/v1/namespaces/app/cronjobs":
		list(map[string]any{"metadata": map[string]any{"namespace": "app", "name": "sync", "uid": "cron"},
			"spec":   map[string]any{"jobTemplate": map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"env": []any{map[string]string{"name": "TOKEN", "value": planted}}}}}}}}},
			"status": map[string]any{"lastSuccessfulTime": recent}})
	case "/apis/external-secrets.io/v1/externalsecrets", "/apis/external-secrets.io/v1/secretstores",
		"/apis/external-secrets.io/v1/clustersecretstores", "/apis/external-secrets.io/v1/clusterexternalsecrets":
		list(map[string]any{"metadata": map[string]any{"namespace": "app", "name": "x", "generation": 1},
			"spec":   map[string]any{"refreshInterval": "0", "data": []any{map[string]string{"secretKey": planted}}},
			"status": map[string]any{"syncedResourceVersion": "1-abc", "conditions": []any{map[string]string{"type": "Ready", "status": "False", "reason": "SecretSyncedError", "message": "provider said " + planted}}}})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(planted))
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func serverCertificate(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "argus"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	write(t, filepath.Join(dir, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	write(t, filepath.Join(dir, "tls.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool
}

func freeAddress(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func TestServeEndToEnd(t *testing.T) {
	dir := t.TempDir()
	api := &fakeAPI{ledger: `{"version":2,"entries":{}}`, version: 1, forbidden: map[string]bool{"/apis/external-secrets.io": true}}
	server := httptest.NewTLSServer(api)
	defer server.Close()
	write(t, filepath.Join(dir, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	write(t, filepath.Join(dir, "sa-token"), []byte("service-account-token"))
	write(t, filepath.Join(dir, "token"), []byte(monitoringToken))
	pool := serverCertificate(t, dir)
	address := freeAddress(t)
	config := map[string]any{"listenAddress": address, "tokenFile": filepath.Join(dir, "token"),
		"tls":                 map[string]string{"certFile": filepath.Join(dir, "tls.crt"), "keyFile": filepath.Join(dir, "tls.key")},
		"pollIntervalSeconds": 1, "collectionTimeoutSeconds": 3, "requestTimeoutSeconds": 2, "maxSampleAgeSeconds": 30,
		"checks": map[string]any{
			"pgbackrest":      map[string]any{"clusters": []any{map[string]any{"namespace": "db", "name": "db", "repos": map[string]any{"repo1": map[string]any{}}}}},
			"cronjobs":        map[string]any{"items": []any{map[string]any{"namespace": "app", "name": "sync", "maxAgeSeconds": 7200}}},
			"externalsecrets": map[string]any{},
		}}
	data, _ := json.Marshal(config)
	write(t, filepath.Join(dir, "config.json"), data)
	var stderr bytes.Buffer
	cmd := exec.Command(binary(t), "serve", "-config", filepath.Join(dir, "config.json"), "-api-url", server.URL,
		"-token-file", filepath.Join(dir, "sa-token"), "-ca-file", filepath.Join(dir, "ca.crt"))
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	get := func(path, token string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "https://"+address+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(req)
		if err != nil {
			return 0, nil
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if strings.Contains(string(body), planted) {
			t.Fatalf("planted secret in %s: %s", path, body)
		}
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return response.StatusCode, out
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, body := get("/v1/pgbackrest", monitoringToken)
		if code == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pgbackrest never became healthy: %d %v\n%s", code, body, stderr.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if code, _ := get("/healthz", ""); code != 200 {
		t.Fatal("healthz", code)
	}
	for _, token := range []string{"", "wrong-token-of-sufficient-length-xx"} {
		if code, _ := get("/v1/checks", token); code != 401 {
			t.Fatal("unauthenticated access", code)
		}
	}
	code, index := get("/v1/checks", monitoringToken)
	results, _ := index["results"].(map[string]any)
	if code != 200 || results["apiserver"] != "ok" || results["sample_fresh"] != "ok" {
		t.Fatal("process gate", code, index)
	}
	// A missing permission fails its own check only.
	code, body := get("/v1/externalsecrets", monitoringToken)
	if code != 503 || body["results"].(map[string]any)["collection"] != "forbidden" {
		t.Fatal(code, body)
	}
	if code, body := get("/v1/cronjobs/app/sync", monitoringToken); code != 200 {
		t.Fatal(code, body)
	}
	// The ledger was written with a conditional PUT and holds the success.
	api.mu.Lock()
	ledger, puts := api.ledger, api.puts
	api.mu.Unlock()
	if puts != 1 || !strings.Contains(ledger, `"db/db/repo1"`) || !strings.Contains(ledger, "PostgresCluster scheduled-backup status") {
		t.Fatal("ledger not persisted once", puts, ledger)
	}
	// An API outage fails closed and trips the gate.
	api.mu.Lock()
	api.down = true
	api.mu.Unlock()
	time.Sleep(3 * time.Second)
	if code, body := get("/v1/cronjobs", monitoringToken); code != 503 || body["results"].(map[string]any)["collection"] != "api_error" {
		t.Fatal(code, body)
	}
	if code, body := get("/v1/checks", monitoringToken); code != 503 || body["results"].(map[string]any)["apiserver"] != "api_error" {
		t.Fatal("an API outage after a success did not trip the gate", code, body["results"])
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("unclean shutdown", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("shutdown hung")
	}
	logs := stderr.String()
	if strings.Contains(logs, planted) || strings.Contains(logs, monitoringToken) || strings.Contains(logs, "service-account-token") {
		t.Fatal("secret in logs", logs)
	}
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatal("unstructured log line", line)
		}
	}
}

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, stdout.String(), stderr.String()
}

func TestOnceAgainstLoopbackProxy(t *testing.T) {
	api := &fakeAPI{ledger: `{"version":2,"entries":{}}`, version: 1}
	proxy := httptest.NewServer(api)
	defer proxy.Close()
	dir := t.TempDir()
	config := `{"checks":{"cronjobs":{"items":[{"namespace":"app","name":"sync","maxAgeSeconds":7200}]},
	"pgbackrest":{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{}}}]}}}`
	write(t, filepath.Join(dir, "config.json"), []byte(config))
	code, stdout, stderr := run(t, "once", "-config", filepath.Join(dir, "config.json"), "-check", "cronjobs", "-api-url", proxy.URL)
	var envelope map[string]any
	if code != 0 || json.Unmarshal([]byte(stdout), &envelope) != nil || envelope["check"] != "cronjobs" || envelope["healthy"] != true {
		t.Fatal(code, stdout, stderr)
	}
	// once never writes the ledger.
	code, stdout, _ = run(t, "once", "-config", filepath.Join(dir, "config.json"), "-check", "pgbackrest", "-api-url", proxy.URL)
	if code != 0 || !strings.Contains(stdout, `"state": "ok"`) || api.puts != 0 {
		t.Fatal("once wrote state or failed", code, stdout, api.puts)
	}
	write(t, filepath.Join(dir, "stale.json"), []byte(strings.Replace(config, "7200", "60", 1)))
	if code, stdout, _ := run(t, "once", "-config", filepath.Join(dir, "stale.json"), "-check", "cronjobs", "-api-url", proxy.URL); code != 1 || !strings.Contains(stdout, "last_success") {
		t.Fatal("unhealthy once did not exit 1", code, stdout)
	}
	// Plain HTTP is accepted only for a loopback origin.
	if code, _, _ := run(t, "once", "-config", filepath.Join(dir, "config.json"), "-check", "cronjobs", "-api-url", "http://192.0.2.1:8001"); code != 2 {
		t.Fatal("non-loopback plain HTTP accepted", code)
	}
	if code, _, _ := run(t, "once", "-config", filepath.Join(dir, "config.json"), "-check", "nodes", "-api-url", proxy.URL); code != 2 {
		t.Fatal("disabled check ran", code)
	}
}

func TestPKILifecycleSnapshot(t *testing.T) {
	dir := t.TempDir()
	cert := func(notAfter, renewal string) string {
		return `{"items":[{"metadata":{"name":"selfsigned-ca","namespace":"cert-manager"},"spec":{"isCA":true,"privateKey":{"rotationPolicy":"Never"}},
		"status":{"notAfter":"` + notAfter + `","renewalTime":"` + renewal + `","conditions":[{"type":"Ready","status":"True"}]}}]}`
	}
	far := time.Now().AddDate(5, 0, 0).UTC().Format(time.RFC3339)
	renew := time.Now().AddDate(4, 0, 0).UTC().Format(time.RFC3339)
	write(t, filepath.Join(dir, "healthy.json"), []byte(cert(far, renew)))
	write(t, filepath.Join(dir, "short.json"), []byte(cert(time.Now().AddDate(0, 0, 100).UTC().Format(time.RFC3339), renew)))
	write(t, filepath.Join(dir, "broken.json"), []byte(`{"kind":"CertificateList"}`))
	if code, stdout, stderr := run(t, "once", "-check", "pki-lifecycle", "-snapshot", filepath.Join(dir, "healthy.json")); code != 0 || !strings.Contains(stdout, `"check": "pki-lifecycle"`) {
		t.Fatal(code, stdout, stderr)
	}
	if code, stdout, _ := run(t, "once", "-check", "pki-lifecycle", "-snapshot", filepath.Join(dir, "short.json")); code != 1 || !strings.Contains(stdout, "issuer_lifetime") {
		t.Fatal(code, stdout)
	}
	if code, _, _ := run(t, "once", "-check", "pki-lifecycle", "-snapshot", filepath.Join(dir, "broken.json")); code != 2 {
		t.Fatal(code)
	}
}

func TestStateValidateAndCheckConfig(t *testing.T) {
	root, _ := filepath.Abs("../..")
	for _, file := range []string{"internal/state/testdata/backrestwatch-v1.json", "internal/state/testdata/backrestwatch-v2.json"} {
		if code, stdout, stderr := run(t, "state", "validate", "-file", filepath.Join(root, file)); code != 0 || !strings.HasPrefix(stdout, "Ledger is valid") {
			t.Fatal(file, code, stdout, stderr)
		}
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, "corrupt.json"), []byte(`{"version":9,"entries":{"x":"`+planted+`"}}`))
	if code, stdout, stderr := run(t, "state", "validate", "-file", filepath.Join(dir, "corrupt.json")); code != 2 || strings.Contains(stdout+stderr, planted) {
		t.Fatal(code, stdout, stderr)
	}
	if code, _, _ := run(t, "serve", "-config", filepath.Join(root, "examples/config.json"), "-check-config"); code != 0 {
		t.Fatal("example config rejected")
	}
	write(t, filepath.Join(dir, "unknown.json"), []byte(`{"checks":{"workloads":{}}}`))
	if code, _, _ := run(t, "serve", "-config", filepath.Join(dir, "unknown.json"), "-check-config"); code != 2 {
		t.Fatal("unknown check accepted", code)
	}
	if code, stdout, _ := run(t, "-version"); code != 0 || strings.TrimSpace(stdout) == "" {
		t.Fatal(code, stdout)
	}
}
