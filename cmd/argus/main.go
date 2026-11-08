// Command argus serves read-only health checks, runs one check from the
// command line, or validates a ledger before import.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zekihan/argus/internal/checks"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
	"github.com/zekihan/argus/internal/result"
	"github.com/zekihan/argus/internal/runner"
	"github.com/zekihan/argus/internal/server"
	"github.com/zekihan/argus/internal/state"
)

var version = "dev"

const usage = `Usage:
  argus serve -config FILE [-check-config]
  argus once -check NAME [-config FILE] [-api-url URL] [-snapshot FILE]
  argus state validate -file FILE
  argus -version`

// errUnhealthy makes once exit 1 without an error message.
var errUnhealthy = errors.New("unhealthy")

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	code := 0
	if err := run(os.Args[1:], os.Stdout, logger); err != nil {
		switch {
		case errors.Is(err, errUnhealthy):
			code = 1
		case errors.Is(err, flag.ErrHelp):
			code = 2
		default:
			logger.Error("Argus stopped", "error", err.Error())
			code = 2
		}
	}
	os.Exit(code)
}

func run(args []string, stdout io.Writer, logger *slog.Logger) error {
	if len(args) == 1 && (args[0] == "-version" || args[0] == "--version" || args[0] == "version") {
		_, err := fmt.Fprintln(stdout, version)
		return err
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		return flag.ErrHelp
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], stdout, logger)
	case "once":
		return once(args[1:], stdout, logger)
	case "state":
		if len(args) < 2 || args[1] != "validate" {
			fmt.Fprintln(os.Stderr, usage)
			return flag.ErrHelp
		}
		return validateState(args[2:], stdout)
	default:
		fmt.Fprintln(os.Stderr, usage)
		return flag.ErrHelp
	}
}

type apiFlags struct {
	url, tokenFile, caFile *string
}

func addAPIFlags(fs *flag.FlagSet) apiFlags {
	return apiFlags{
		url:       fs.String("api-url", "", "Kubernetes HTTPS origin, or a loopback kubectl proxy such as http://127.0.0.1:8001; defaults to the in-cluster environment"),
		tokenFile: fs.String("token-file", "/var/run/secrets/kubernetes.io/serviceaccount/token", "Projected service-account token"),
		caFile:    fs.String("ca-file", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", "Kubernetes CA bundle"),
	}
}

func (f apiFlags) client(timeout time.Duration) (*kube.Client, error) {
	origin := *f.url
	if origin == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" || port == "" {
			return nil, fmt.Errorf("set -api-url or the in-cluster Kubernetes service environment")
		}
		origin = "https://" + net.JoinHostPort(host, port)
	}
	return kube.New(origin, *f.tokenFile, *f.caFile, version, timeout)
}

func serve(args []string, stdout io.Writer, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configFile := fs.String("config", "/config/config.json", "Strict JSON configuration file")
	checkConfig := fs.Bool("check-config", false, "Validate configuration and exit without contacting Kubernetes")
	api := addAPIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}
	if *checkConfig {
		_, err := fmt.Fprintln(stdout, "Configuration is valid")
		return err
	}
	var client *kube.Client
	var health runner.HealthSource
	if checks.Kubernetes(cfg) {
		if client, err = api.client(time.Duration(cfg.RequestTimeoutSeconds) * time.Second); err != nil {
			return err
		}
		defer client.HTTP.CloseIdleConnections()
		health = client
	}
	built, err := checks.Build(cfg, checks.Deps{API: client, Logger: logger})
	if err != nil {
		return err
	}
	r := runner.New(logger, version, health, time.Duration(cfg.MaxSampleAgeSeconds)*time.Second)
	if client != nil {
		r.ProbeEvery(client, time.Duration(cfg.PollIntervalSeconds)*time.Second, time.Duration(cfg.RequestTimeoutSeconds)*time.Second)
	}
	for _, b := range built {
		r.Add(b.Check, b.Timing)
	}
	var certificates *server.Reloader
	if cfg.TLS != nil {
		if certificates, err = server.NewReloader(cfg.TLS.CertFile, cfg.TLS.KeyFile, 30*time.Second, logger); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	names := make([]string, 0, len(built))
	for _, b := range built {
		names = append(names, b.Check.Name())
	}
	logger.Info("Starting argus", "version", version, "checks", names, "address", cfg.ListenAddress, "tls", cfg.TLS != nil)
	return server.Serve(ctx, listener, r.Run, r, cfg.TokenFile, certificates, logger)
}

func once(args []string, stdout io.Writer, logger *slog.Logger) error {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	configFile := fs.String("config", "/config/config.json", "Strict JSON configuration file")
	name := fs.String("check", "", "Check to run: a configured check name, or pki-lifecycle")
	snapshot := fs.String("snapshot", "", "pki-lifecycle only: offline CertificateList JSON (kubectl get certificates -A -o json)")
	api := addAPIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("once requires -check")
	}
	if *name == "pki-lifecycle" {
		return pkiLifecycle(*snapshot, api, stdout)
	}
	if *snapshot != "" {
		return fmt.Errorf("-snapshot applies only to pki-lifecycle")
	}
	cfg, err := config.Load(*configFile)
	if err != nil {
		return err
	}
	var selected []config.EnabledCheck
	for _, enabled := range cfg.Enabled() {
		if enabled.Name == *name {
			selected = append(selected, enabled)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("check %s is not enabled in the configuration", *name)
	}
	cfg.Checks = onlyCheck(cfg.Checks, *name)
	var client *kube.Client
	if checks.Kubernetes(cfg) {
		if client, err = api.client(time.Duration(cfg.RequestTimeoutSeconds) * time.Second); err != nil {
			return err
		}
		defer client.HTTP.CloseIdleConnections()
	}
	quiet := slog.New(slog.NewJSONHandler(io.Discard, nil))
	built, err := checks.Build(cfg, checks.Deps{API: client, Logger: logger, ReadOnly: true})
	if err != nil {
		return err
	}
	r := runner.New(quiet, version, nil, 0)
	r.Add(built[0].Check, built[0].Timing)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r.Refresh(ctx, *name)
	envelopes := r.Envelopes(*name, time.Now())
	healthy := true
	for _, e := range envelopes {
		healthy = healthy && e.Healthy
	}
	var output any = envelopes[0]
	if len(envelopes) > 1 {
		output = struct {
			Check       string            `json:"check"`
			Healthy     bool              `json:"healthy"`
			Collections []result.Envelope `json:"collections"`
		}{*name, healthy, envelopes}
	}
	if err := printJSON(stdout, output); err != nil {
		return err
	}
	if !healthy {
		return errUnhealthy
	}
	return nil
}

func onlyCheck(all config.Checks, name string) config.Checks {
	var only config.Checks
	switch name {
	case "hosts":
		only.Hosts = all.Hosts
	case "nodes":
		only.Nodes = all.Nodes
	case "postgres":
		only.Postgres = all.Postgres
	case "pgbackrest":
		only.PGBackRest = all.PGBackRest
	case "longhorn":
		only.Longhorn = all.Longhorn
	case "cronjobs":
		only.CronJobs = all.CronJobs
	case "certificates":
		only.Certificates = all.Certificates
	case "pki":
		only.PKI = all.PKI
	case "externalsecrets":
		only.ExternalSecrets = all.ExternalSecrets
	case "argocd":
		only.ArgoCD = all.ArgoCD
	}
	return only
}

func printJSON(stdout io.Writer, value any) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func validateState(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("state validate", flag.ContinueOnError)
	file := fs.String("file", "", "Exported ledger (backrestwatch evidence.json) to validate before import")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("state validate requires -file")
	}
	ledger, err := state.ValidateFile(*file)
	if err != nil {
		return err
	}
	data, err := state.Encode(ledger)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Ledger is valid: %d entries, %d bytes as a version 2 document (limit %d)\n", len(ledger), len(data), state.MaxBytes)
	return err
}
