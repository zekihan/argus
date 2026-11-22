package checks

import (
	"log/slog"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/config"
)

func parse(t *testing.T, json string) config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(json))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const dohJSON = `{"checks":{"doh":{"url":"https://dns.example.com/dns-query","name":"example.com","expectedIPv4":"192.0.2.1","intervalSeconds":120,"collectionTimeoutSeconds":30,"maxSampleAgeSeconds":600}}}`

func TestBuildConstructsEnabledChecksInOrderWithTiming(t *testing.T) {
	cfg := parse(t, `{"checks":{"doh":{"url":"https://dns.example.com/dns-query","name":"example.com","expectedIPv4":"192.0.2.1","intervalSeconds":120,"collectionTimeoutSeconds":30,"maxSampleAgeSeconds":600},"hosts":{"privateKeyFile":"/tmp/key","knownHostsFile":"/tmp/known","hosts":[{"name":"bastion","address":"192.0.2.10"}]}}}`)
	built, err := Build(cfg, Deps{Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 2 || built[0].Check.Name() != "doh" || built[1].Check.Name() != "hosts" {
		t.Fatalf("built = %v, want [doh hosts]", built)
	}
	if want := (time.Duration(120) * time.Second); built[0].Timing.Interval != want {
		t.Fatalf("doh interval = %v, want %v", built[0].Timing.Interval, want)
	}
	if want := (30 * time.Second); built[0].Timing.Timeout != want {
		t.Fatalf("doh timeout = %v, want %v", built[0].Timing.Timeout, want)
	}
	if want := (600 * time.Second); built[0].Timing.MaxAge != want {
		t.Fatalf("doh maxAge = %v, want %v", built[0].Timing.MaxAge, want)
	}
	// Hosts has no schedule override, so it inherits the global timing.
	if want := (60 * time.Second); built[1].Timing.Interval != want {
		t.Fatalf("hosts interval = %v, want %v", built[1].Timing.Interval, want)
	}
}

func TestBuildWithNoChecksReturnsEmpty(t *testing.T) {
	built, err := Build(parse(t, `{}`), Deps{Logger: slog.New(slog.DiscardHandler)})
	if err != nil || len(built) != 0 {
		t.Fatalf("built = %v, err = %v, want empty and nil", built, err)
	}
}

func TestKubernetes(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
		want bool
	}{
		{"no checks", `{}`, false},
		{"doh only", dohJSON, false},
		{"hosts only", `{"checks":{"hosts":{"privateKeyFile":"/tmp/key","knownHostsFile":"/tmp/known","hosts":[{"name":"bastion","address":"192.0.2.10"}]}}}`, false},
		{"doh and hosts", `{"checks":{"doh":{"url":"https://dns.example.com/dns-query","name":"example.com","expectedIPv4":"192.0.2.1"},"hosts":{"privateKeyFile":"/tmp/key","knownHostsFile":"/tmp/known","hosts":[{"name":"bastion","address":"192.0.2.10"}]}}}`, false},
		{"nodes needs the API", `{"checks":{"nodes":{"expected":["node-a"]}}}`, true},
		{"doh plus cronjobs needs the API", `{"checks":{"doh":{"url":"https://dns.example.com/dns-query","name":"example.com","expectedIPv4":"192.0.2.1"},"cronjobs":{"items":[{"namespace":"app","name":"job","maxAgeSeconds":60}]}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Kubernetes(parse(t, tc.json)); got != tc.want {
				t.Fatalf("Kubernetes() = %v, want %v", got, tc.want)
			}
		})
	}
}
