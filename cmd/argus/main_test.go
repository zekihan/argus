package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunRejectsUnexpectedArguments(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	ledgerFile := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(configFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerFile, []byte(`{"version":2,"entries":{}}`), 0600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		command string
		args    []string
	}{
		{"serve", []string{"serve", "-config", configFile, "-check-config", "extra"}},
		{"once", []string{"once", "-config", configFile, "extra", "-check", "externalsecrets"}},
		{"state validate", []string{"state", "validate", "-file", ledgerFile, "--", "extra"}},
	} {
		t.Run(tc.command, func(t *testing.T) {
			var stdout bytes.Buffer
			err := run(tc.args, &stdout, slog.New(slog.DiscardHandler))
			want := tc.command + " does not take positional arguments"
			if err == nil || err.Error() != want || stdout.Len() != 0 {
				t.Fatalf("error = %v, stdout = %q, want %q and empty stdout", err, stdout.String(), want)
			}
		})
	}
}

func TestRunVersionAndUsage(t *testing.T) {
	for _, arg := range []string{"-version", "--version", "version"} {
		var stdout bytes.Buffer
		if err := run([]string{arg}, &stdout, slog.New(slog.DiscardHandler)); err != nil || stdout.String() != version+"\n" {
			t.Fatalf("run(%q) = %v, %q, want the version", arg, err, stdout.String())
		}
	}
	for _, args := range [][]string{{}, {"bogus"}, {"state"}, {"state", "bogus"}, {"-version", "extra"}} {
		var stdout bytes.Buffer
		if err := run(args, &stdout, slog.New(slog.DiscardHandler)); !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("run(%v) = %v, want flag.ErrHelp", args, err)
		}
	}
}

func TestValidateStateCommand(t *testing.T) {
	dir := t.TempDir()
	ledgerFile := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(ledgerFile, []byte(`{"version":2,"entries":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := run([]string{"state", "validate", "-file", ledgerFile}, &stdout, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout.String(), "Ledger is valid: 0 entries, ") {
		t.Fatalf("stdout = %q, want a valid-ledger report", stdout.String())
	}
	if err := run([]string{"state", "validate"}, &bytes.Buffer{}, slog.New(slog.DiscardHandler)); err == nil || err.Error() != "state validate requires -file" {
		t.Fatalf("missing -file = %v, want a requires-file error", err)
	}
	badFile := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badFile, []byte(`{"version":9,"entries":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"state", "validate", "-file", badFile}, &bytes.Buffer{}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("an unknown ledger version must fail validation")
	}
	if err := run([]string{"state", "validate", "-file", filepath.Join(dir, "missing.json")}, &bytes.Buffer{}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a missing ledger file must fail validation")
	}
}

func TestOnceCommandErrors(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configFile, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing check", []string{"once", "-config", configFile}, "once requires -check"},
		{"snapshot on a regular check", []string{"once", "-config", configFile, "-check", "cronjobs", "-snapshot", "x.json"}, "-snapshot applies only to pki-lifecycle"},
		{"check not enabled", []string{"once", "-config", configFile, "-check", "cronjobs"}, "check cronjobs is not enabled in the configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := run(tc.args, &bytes.Buffer{}, logger); err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	if err := run([]string{"serve", "-config", configFile, "-check-config"}, &bytes.Buffer{}, logger); err != nil {
		t.Fatalf("serve -check-config on an empty configuration = %v, want nil", err)
	}
}

func TestPKILifecycleSnapshot(t *testing.T) {
	dir := t.TempDir()
	notAfter := time.Now().Add(200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	renewal := time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	snapshot := `{"items":[{"metadata":{"namespace":"cert-manager","name":"selfsigned-ca"},"spec":{"isCA":true,"issuerRef":{"name":"selfsigned-ca-issuer"},"privateKey":{"rotationPolicy":"Never"}},"status":{"notAfter":"` + notAfter + `","renewalTime":"` + renewal + `","conditions":[{"type":"Ready","status":"True"}]}}]}`
	snapshotFile := filepath.Join(dir, "certificates.json")
	if err := os.WriteFile(snapshotFile, []byte(snapshot), 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.DiscardHandler)
	var stdout bytes.Buffer
	if err := run([]string{"once", "-check", "pki-lifecycle", "-snapshot", snapshotFile}, &stdout, logger); err != nil {
		t.Fatalf("healthy snapshot = %v, stdout = %s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"healthy": true`) {
		t.Fatalf("stdout = %s, want a healthy envelope", stdout.String())
	}
	emptyFile := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(emptyFile, []byte(`{"items":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := run([]string{"once", "-check", "pki-lifecycle", "-snapshot", emptyFile}, &stdout, logger); !errors.Is(err, errUnhealthy) {
		t.Fatalf("snapshot without the root = %v, want errUnhealthy", err)
	}
	for name, content := range map[string]string{"not a list": `{"items":null}`, "malformed": `{"items":`} {
		badFile := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(badFile, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := run([]string{"once", "-check", "pki-lifecycle", "-snapshot", badFile}, &bytes.Buffer{}, logger); err == nil || err.Error() != "snapshot is not a CertificateList" {
			t.Fatalf("%s snapshot = %v, want a CertificateList error", name, err)
		}
	}
	if err := run([]string{"once", "-check", "pki-lifecycle", "-snapshot", filepath.Join(dir, "missing.json")}, &bytes.Buffer{}, logger); err == nil || err.Error() != "open snapshot: file does not exist" {
		t.Fatalf("missing snapshot = %v, want a file-does-not-exist error", err)
	}
	huge := filepath.Join(dir, "huge.json")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(64*1024*1024 + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"once", "-check", "pki-lifecycle", "-snapshot", huge}, &bytes.Buffer{}, logger); err == nil || err.Error() != "snapshot exceeds read budget or cannot be read" {
		t.Fatalf("oversized snapshot = %v", err)
	}
}

func TestAPIClient(t *testing.T) {
	without := flag.NewFlagSet("once", flag.ContinueOnError)
	api := addAPIFlags(without)
	if err := without.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := api.client(time.Second); err == nil || err.Error() != "set -api-url or the in-cluster Kubernetes service environment" {
		t.Fatalf("missing origin = %v", err)
	}
	inCluster := flag.NewFlagSet("once", flag.ContinueOnError)
	clusterAPI := addAPIFlags(inCluster)
	if err := inCluster.Parse([]string{"-ca-file", filepath.Join(t.TempDir(), "missing.crt")}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	_, err := clusterAPI.client(time.Second)
	if err == nil || strings.Contains(err.Error(), "10.0.0.1") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("in-cluster client = %v, want a CA failure that does not echo the API address", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	withURL := flag.NewFlagSet("once", flag.ContinueOnError)
	loopback := addAPIFlags(withURL)
	if err := withURL.Parse([]string{"-api-url", server.URL}); err != nil {
		t.Fatal(err)
	}
	client, err := loopback.client(time.Second)
	if err != nil || client.BaseURL != server.URL {
		t.Fatal(client, err)
	}
	bad := flag.NewFlagSet("once", flag.ContinueOnError)
	plain := addAPIFlags(bad)
	if err := bad.Parse([]string{"-api-url", "http://example.com:80"}); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.client(time.Second); err == nil {
		t.Fatal("a non-loopback HTTP origin was accepted")
	}
}

func TestOnceFailsClosedForEveryCheck(t *testing.T) {
	const secret = "secret-api-body"
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, secret)
	}))
	defer forbidden.Close()
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/configmaps/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, secret)
			return
		}
		if r.URL.Path == "/version" {
			_, _ = io.WriteString(w, `{"gitVersion":"v1.31.0"}`)
			return
		}
		_, _ = io.WriteString(w, `{"metadata":{"resourceVersion":"1"},"items":[]}`)
	}))
	defer empty.Close()

	dir := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	sha := strings.Repeat("ab", 32)
	configs := map[string]string{
		"doh":             `{"checks":{"doh":{"url":"https://127.0.0.1:1/dns-query","name":"example.com","expectedIPv4":"192.0.2.1"}}}`,
		"pvcs":            `{"checks":{"pvcs":{"storageClasses":["longhorn"],"expectedNodes":["worker"],"expected":[{"namespace":"db","name":"data"}]}}}`,
		"hosts":           `{"checks":{"hosts":{"privateKeyFile":"/no/such/key","knownHostsFile":"/no/such/known","hosts":[{"name":"bastion","address":"127.0.0.1"}]}}}`,
		"nodes":           `{"checks":{"nodes":{"expected":["worker"]}}}`,
		"postgres":        `{"checks":{"postgres":{"clusters":[{"namespace":"db","name":"db"}]}}}`,
		"pgbackrest":      `{"checks":{"pgbackrest":{"clusters":[{"namespace":"db","name":"db","repos":{"repo1":{}}}]}}}`,
		"longhorn":        `{"checks":{"longhorn":{"namespace":"longhorn","expectedPVCs":["db/data"],"maxBackupAgeSeconds":86400,"systemBackupMaxAgeSeconds":86400}}}`,
		"cronjobs":        `{"checks":{"cronjobs":{"items":[{"namespace":"app","name":"job","maxAgeSeconds":3600}]}}}`,
		"certificates":    `{"checks":{"certificates":{"expected":[{"namespace":"web","name":"site"}]}}}`,
		"pki":             `{"checks":{"pki":{"rootSHA256":"` + sha + `","expectedCertificates":[{"namespace":"cert-manager","name":"selfsigned-ca"}]}}}`,
		"externalsecrets": `{"checks":{"externalsecrets":{"expectedExternalSecrets":[{"namespace":"apps","name":"example"}]}}}`,
		"argocd":          `{"checks":{"argocd":{"namespace":"argocd","expected":["root"]}}}`,
		"pbs":             `{"checks":{"pbs":{"url":"https://127.0.0.1:1","tokenFile":"/no/such/pbs-token","fingerprint":"` + strings.Repeat("ab", 32) + `"}}}`,
	}
	nonAPI := map[string]bool{"doh": true, "hosts": true, "pbs": true}
	for name, body := range configs {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(dir, name+".json")
			if err := os.WriteFile(file, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"once", "-config", file, "-check", name}
			if !nonAPI[name] {
				args = append(args, "-api-url", forbidden.URL)
			}
			var stdout bytes.Buffer
			err := run(args, &stdout, logger)
			if !errors.Is(err, errUnhealthy) || strings.Contains(stdout.String(), secret) || strings.Contains(stdout.String(), "no such file") {
				t.Fatalf("error = %v, stdout = %s", err, stdout.String())
			}
			if !nonAPI[name] && !strings.Contains(stdout.String(), `"collection": "forbidden"`) {
				t.Fatalf("stdout = %s, want a forbidden collection", stdout.String())
			}
			if name == "pbs" && !strings.Contains(stdout.String(), "token_unreadable") {
				t.Fatalf("stdout = %s, want token_unreadable", stdout.String())
			}
		})
	}

	missingFile := filepath.Join(dir, "nodes.json")
	var stdout bytes.Buffer
	if err := run([]string{"once", "-config", missingFile, "-check", "nodes", "-api-url", empty.URL}, &stdout, logger); !errors.Is(err, errUnhealthy) || !strings.Contains(stdout.String(), "missing(1): worker") || strings.Contains(stdout.String(), secret) {
		t.Fatalf("missing node = %v, stdout = %s", err, stdout.String())
	}
	stdout.Reset()
	healthyFile := filepath.Join(dir, "externalsecrets.json")
	if err := os.WriteFile(healthyFile, []byte(`{"checks":{"externalsecrets":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"once", "-config", healthyFile, "-check", "externalsecrets", "-api-url", empty.URL}, &stdout, logger); err != nil || !strings.Contains(stdout.String(), `"healthy": true`) || !strings.Contains(stdout.String(), `"collections"`) {
		t.Fatalf("healthy once = %v, stdout = %s", err, stdout.String())
	}
}

func TestPKILifecycleFromAPI(t *testing.T) {
	const secret = "secret-api-body"
	notAfter := time.Now().Add(200 * 24 * time.Hour).UTC().Format(time.RFC3339)
	renewal := time.Now().Add(100 * 24 * time.Hour).UTC().Format(time.RFC3339)
	list := `{"metadata":{"resourceVersion":"1"},"items":[{"metadata":{"namespace":"cert-manager","name":"selfsigned-ca"},"spec":{"isCA":true,"issuerRef":{"name":"selfsigned-ca-issuer"},"privateKey":{"rotationPolicy":"Never"}},"status":{"notAfter":"` + notAfter + `","renewalTime":"` + renewal + `","conditions":[{"type":"Ready","status":"True"}]}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/cert-manager.io/v1/certificates" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, secret)
			return
		}
		_, _ = io.WriteString(w, list)
	}))
	defer server.Close()
	logger := slog.New(slog.DiscardHandler)
	var stdout bytes.Buffer
	if err := run([]string{"once", "-check", "pki-lifecycle", "-api-url", server.URL}, &stdout, logger); err != nil || !strings.Contains(stdout.String(), `"healthy": true`) || strings.Contains(stdout.String(), secret) {
		t.Fatalf("api lifecycle = %v, stdout = %s", err, stdout.String())
	}
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, secret)
	}))
	defer denied.Close()
	err := run([]string{"once", "-check", "pki-lifecycle", "-api-url", denied.URL}, &bytes.Buffer{}, logger)
	if err == nil || errors.Is(err, errUnhealthy) || strings.Contains(err.Error(), secret) {
		t.Fatalf("forbidden lifecycle = %v", err)
	}
}

func TestServeDecisionPaths(t *testing.T) {
	dir := t.TempDir()
	logger := slog.New(slog.DiscardHandler)
	nodes := filepath.Join(dir, "nodes.json")
	if err := os.WriteFile(nodes, []byte(`{"checks":{"nodes":{"expected":["worker"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"serve", "-config", nodes}, &bytes.Buffer{}, logger); err == nil || err.Error() != "set -api-url or the in-cluster Kubernetes service environment" {
		t.Fatalf("serve without an API = %v", err)
	}
	doh := filepath.Join(dir, "doh.json")
	if err := os.WriteFile(doh, []byte(`{"listenAddress":"127.0.0.1:1","tls":{"certFile":"/no/such/cert","keyFile":"/no/such/key"},"checks":{"doh":{"url":"https://127.0.0.1:1/dns-query","name":"example.com","expectedIPv4":"192.0.2.1"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"serve", "-config", doh}, &bytes.Buffer{}, logger); err == nil {
		t.Fatal("missing TLS material was accepted")
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	busy := filepath.Join(dir, "busy.json")
	if err := os.WriteFile(busy, []byte(fmt.Sprintf(`{"listenAddress":%q,"checks":{"doh":{"url":"https://127.0.0.1:1/dns-query","name":"example.com","expectedIPv4":"192.0.2.1"}}}`, held.Addr().String())), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"serve", "-config", busy}, &bytes.Buffer{}, logger); err == nil || err.Error() != "cannot listen on "+held.Addr().String() {
		t.Fatalf("busy address = %v", err)
	}
	metricsHeld, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer metricsHeld.Close()
	metricsBusy := filepath.Join(dir, "metrics-busy.json")
	if err := os.WriteFile(metricsBusy, []byte(fmt.Sprintf(`{"listenAddress":"127.0.0.1:%d","metricsAddress":%q,"checks":{"doh":{"url":"https://127.0.0.1:1/dns-query","name":"example.com","expectedIPv4":"192.0.2.1"}}}`, freePort(t), metricsHeld.Addr().String())), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"serve", "-config", metricsBusy}, &bytes.Buffer{}, logger); err == nil || err.Error() != "cannot listen on metrics "+metricsHeld.Addr().String() {
		t.Fatalf("busy metrics address = %v", err)
	}

	const secret = "secret-api-body"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, secret)
	}))
	defer api.Close()
	port := freePort(t)
	token := strings.Repeat("a", 32)
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	serveFile := filepath.Join(dir, "serve.json")
	if err := os.WriteFile(serveFile, []byte(fmt.Sprintf(`{"listenAddress":"127.0.0.1:%d","tokenFile":%q,"checks":{"nodes":{"expected":["worker"]}}}`, port, tokenFile)), 0600); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- run([]string{"serve", "-config", serveFile, "-api-url", api.URL}, &bytes.Buffer{}, logger)
	}()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	health := waitGET(t, base+"/healthz", "")
	if !strings.Contains(health, `"healthy":true`) {
		t.Fatal(health)
	}
	body := waitGET(t, base+"/v1/nodes", token)
	if !strings.Contains(body, `"collection":"forbidden"`) || strings.Contains(body, secret) {
		t.Fatalf("nodes = %s", body)
	}
	// The default metrics listener serves collection health and request
	// counts on :9090/metrics without authentication.
	metricsDeadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get("http://127.0.0.1:9090/metrics") //nolint:noctx,bodyclose // test-only loop with a deadline
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if strings.Contains(string(data), `argus_collection_healthy{collection="nodes"}`) &&
				strings.Contains(string(data), `http_requests_total{method="GET",route="/healthz",status="200"}`) {
				break
			}
		}
		if time.Now().After(metricsDeadline) {
			t.Fatal("metrics missing collection health or request counts")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after SIGTERM")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func waitGET(t *testing.T, url, token string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable && token != "" && !strings.Contains(string(data), "forbidden") {
			last = errors.New(string(data))
			time.Sleep(20 * time.Millisecond)
			continue
		}
		return string(data)
	}
	t.Fatal(last)
	return ""
}
