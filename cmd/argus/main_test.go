package main

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
}
