package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
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
