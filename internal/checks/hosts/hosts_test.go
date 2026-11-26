package hosts

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/result"
)

// fields is the check-host.sh JSON contract (apps/gatus/tests/test_ssh.py).
var fields = config.HostDimensions

func allOK() map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		out[f] = "ok"
	}
	return out
}

func probeJSON(t *testing.T, values map[string]string) []byte {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

type behavior struct {
	stdout   []byte
	stderr   string
	exit     uint32
	hang     bool
	stallAt  string
	onStall  func()
	rejectAt string
}

type server struct {
	address  string
	mu       sync.Mutex
	requests []string
}

// signer wraps the ssh.Signer usable as a concrete return value.
type signer struct {
	ssh.Signer
}

func newSigner(t *testing.T) signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer{inner}
}

// startServer is an SSH server fixture that authorizes one client key and
// answers a session the way a forced command would.
// handleChannel serves one session: record requests and reply per behavior.
func (s *server) handleChannel(channel ssh.Channel, reqs <-chan *ssh.Request, b behavior) {
	if b.stallAt == "session" {
		b.onStall()
		return
	}
	for req := range reqs {
		s.mu.Lock()
		s.requests = append(s.requests, req.Type+":"+string(req.Payload))
		s.mu.Unlock()
		if req.Type != "shell" && req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		if b.stallAt == req.Type {
			b.onStall()
			continue
		}
		if b.rejectAt == req.Type {
			_ = req.Reply(false, nil)
			continue
		}
		_ = req.Reply(true, nil)
		if b.hang {
			<-time.After(2 * time.Second)
		}
		_, _ = channel.Write(b.stdout)
		_, _ = channel.Stderr().Write([]byte(b.stderr))
		status := make([]byte, 4)
		binary.BigEndian.PutUint32(status, b.exit)
		_, _ = channel.SendRequest("exit-status", false, status)
		_ = channel.Close()
	}
}

func (s *server) serveConn(conn net.Conn, cfg *ssh.ServerConfig, b behavior) {
	defer conn.Close()
	_, channels, requests, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if b.rejectAt == newChannel.ChannelType() {
			_ = newChannel.Reject(ssh.Prohibited, "channel refused")
			continue
		}
		if b.stallAt == "session" {
			b.onStall()
			continue
		}
		channel, reqs, err := newChannel.Accept()
		if err != nil {
			return
		}
		go s.handleChannel(channel, reqs, b)
	}
}

func startServer(t *testing.T, host ssh.Signer, authorized ssh.PublicKey, b behavior) *server {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	s := &server{address: listener.Addr().String()}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, os.ErrPermission
		},
	}
	cfg.AddHostKey(host)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.serveConn(conn, cfg, b)
		}
	}()
	return s
}

type fixture struct {
	dir       string
	client    ssh.Signer
	host      ssh.Signer
	keyFile   string
	knownFile string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		dir:       dir,
		client:    newSigner(t),
		host:      newSigner(t),
		keyFile:   filepath.Join(dir, "id"),
		knownFile: filepath.Join(dir, "known_hosts"),
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	f.client, err = ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.knownFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) pin(t *testing.T, address string, key ssh.PublicKey) {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(address)}, key) + "\n"
	file, err := os.OpenFile(f.knownFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) prober() SSH {
	return SSH{User: "argus", PrivateKeyFile: f.keyFile, KnownHostsFile: f.knownFile}
}

func probe(t *testing.T, f *fixture, address string, expects ...string) result.Item {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	host := config.Host{Name: "host", Address: address, Expects: expects}
	return Evaluate(host, f.prober().Probe(ctx, host))
}

func TestKnownUnknownAndChangedHostKeys(t *testing.T) {
	f := newFixture(t)
	s := startServer(t, f.host, f.client.PublicKey(), behavior{stdout: probeJSON(t, allOK())})
	if item := probe(
		t,
		f,
		s.address,
	); item.Results["ssh"] != "host_key_unknown" ||
		item.Results["disk"] != "unknown" {
		t.Fatal("unknown host key accepted", item.Results)
	}
	f.pin(t, s.address, newSigner(t).PublicKey())
	if item := probe(t, f, s.address); item.Results["ssh"] != "host_key_mismatch" {
		t.Fatal("changed host key accepted", item.Results)
	}
	if err := os.WriteFile(f.knownFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.pin(t, s.address, f.host.PublicKey())
	item := probe(t, f, s.address)
	for _, dim := range dimensions {
		if item.Results[dim] != "ok" {
			t.Fatal("known host failed", item.Results)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 1 || s.requests[0] != "shell:" {
		t.Fatal("a command or script was sent", s.requests)
	}
}

func TestAuthenticationAndTransportFailures(t *testing.T) {
	f := newFixture(t)
	s := startServer(t, f.host, newSigner(t).PublicKey(), behavior{stdout: probeJSON(t, allOK())})
	f.pin(t, s.address, f.host.PublicKey())
	if item := probe(t, f, s.address); item.Results["ssh"] != "auth_failed" {
		t.Fatal(item.Results)
	}
	closed, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := closed.Addr().String()
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	f.pin(t, address, f.host.PublicKey())
	if item := probe(t, f, address); item.Results["ssh"] != "unreachable" {
		t.Fatal(item.Results)
	}
	if err := os.WriteFile(f.keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if item := probe(t, f, s.address); item.Results["ssh"] != "key_unavailable" {
		t.Fatal(item.Results)
	}
}

func TestProbeExitOutputAndTimeout(t *testing.T) {
	failing := allOK()
	failing["disk"] = "/a\"b\\c=95%\n/other=90% (threshold: 90%)"
	failing["zfs"] = "n/a"
	failing["ups"] = "unavailable"
	for _, tc := range []struct {
		name string
		b    behavior
		dim  string
		want string
	}{
		{"exit 1 with results", behavior{stdout: probeJSON(t, failing), exit: 1}, "disk", `/a"b\c=95% /other=90% (threshold: 90%)`},
		{"missing tool", behavior{stdout: probeJSON(t, failing), exit: 1}, "zfs", "n/a"},
		{"failing tool", behavior{stdout: probeJSON(t, failing), exit: 1}, "ups", "unavailable"},
		{"exit 3", behavior{stdout: probeJSON(t, allOK()), exit: 3, stderr: "sudo: password=hunter2"}, "script", "exit_3"},
		{"invalid output", behavior{stdout: []byte("sudo: a password is required\n"), exit: 1}, "script", "invalid_output"},
		{"oversized output", behavior{stdout: []byte(strings.Repeat("x", maxOutput+1))}, "script", "output_too_large"},
		{"timeout", behavior{hang: true, stdout: probeJSON(t, allOK())}, "script", "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			s := startServer(t, f.host, f.client.PublicKey(), tc.b)
			f.pin(t, s.address, f.host.PublicKey())
			item := probe(t, f, s.address)
			if tc.name == "timeout" && item.Results["ssh"] != "ok" {
				// A timeout during the handshake is also acceptable.
				if item.Results["ssh"] != "timeout" {
					t.Fatal(item.Results)
				}
				return
			}
			if item.Results[tc.dim] != tc.want {
				t.Fatal(item.Results)
			}
		})
	}
}

func TestProbeCancellationDuringSessionSetup(t *testing.T) {
	for _, phase := range []string{"session", "shell"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s := startServer(
				t,
				f.host,
				f.client.PublicKey(),
				behavior{stallAt: phase, onStall: cancel},
			)
			f.pin(t, s.address, f.host.PublicKey())
			host := config.Host{Name: "host", Address: s.address}
			item := Evaluate(host, f.prober().Probe(ctx, host))
			if item.Results["ssh"] != "ok" || item.Results["script"] != "timeout" ||
				item.Results["disk"] != "unknown" {
				t.Fatalf("unexpected results after cancellation during %s: %v", phase, item.Results)
			}
		})
	}
}

func TestExpectedDimensionsTurnMissingToolsUnavailable(t *testing.T) {
	values := allOK()
	values["zfs"], values["ups"] = "n/a", "n/a"
	item := Evaluate(
		config.Host{Name: "pve", Expects: []string{"zfs"}},
		Outcome{SSH: "ok", Exit: 0, Output: probeJSON(t, values)},
	)
	if item.Results["zfs"] != "unavailable" || item.Results["ups"] != "n/a" {
		t.Fatal(item.Results)
	}
	delete(values, "swap")
	item = Evaluate(
		config.Host{Name: "pve"},
		Outcome{SSH: "ok", Exit: 0, Output: probeJSON(t, values)},
	)
	if item.Results["swap"] != "unknown" {
		t.Fatal("missing probe field passed", item.Results)
	}
	item = Evaluate(
		config.Host{Name: "pve"},
		Outcome{SSH: "ok", Exit: 0, Output: []byte(`{"disk":1}`)},
	)
	if item.Results["disk"] != "unknown" {
		t.Fatal(item.Results)
	}
	long := allOK()
	long["failed_units"] = strings.Repeat("unit.service ", 100)
	item = Evaluate(
		config.Host{Name: "pve"},
		Outcome{SSH: "ok", Exit: 1, Output: probeJSON(t, long)},
	)
	if len(item.Results["failed_units"]) > maxFieldLength {
		t.Fatal("probe field not capped")
	}
}

func TestProbeFailuresStayOnFixedCodes(t *testing.T) {
	f := newFixture(t)
	if err := os.Remove(f.keyFile); err != nil {
		t.Fatal(err)
	}
	if item := probe(
		t,
		f,
		"127.0.0.1:1",
	); item.Results["ssh"] != "key_unavailable" ||
		strings.Contains(item.Results["ssh"], "no such") {
		t.Fatal(item.Results)
	}
	f = newFixture(t)
	if err := os.WriteFile(f.keyFile, []byte(strings.Repeat("a", 64*1024+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if item := probe(t, f, "127.0.0.1:1"); item.Results["ssh"] != "key_unavailable" {
		t.Fatal(item.Results)
	}
	f = newFixture(t)
	host := config.Host{Name: "bad", Address: "not a host"}
	if item := Evaluate(
		host,
		f.prober().Probe(t.Context(), host),
	); item.Results["ssh"] != "unreachable" {
		t.Fatal(item.Results)
	}
	if item := Evaluate(
		host,
		(SSH{User: "argus", PrivateKeyFile: f.keyFile, KnownHostsFile: f.dir}).Probe(
			t.Context(),
			host,
		),
	); item.Results["ssh"] != "known_hosts_unavailable" {
		t.Fatal(item.Results)
	}
	if item := Evaluate(
		config.Host{Name: "pve"},
		Outcome{SSH: result.OK, Exit: -1},
	); item.Results["script"] != "killed" ||
		item.Results["disk"] != "unknown" {
		t.Fatal(item.Results)
	}
	if item := Evaluate(
		config.Host{Name: "pve"},
		Outcome{SSH: result.OK, Exit: 0, Output: []byte(`{"disk":"ok "}`)},
	); item.Results["disk"] != "unknown" {
		t.Fatal(item.Results)
	}
	if item := Evaluate(
		config.Host{Name: "pve"},
		Outcome{SSH: result.OK, Exit: 0, Output: []byte("{\"disk\":\"\u0000\"}")},
	); item.Results["disk"] != "unknown" {
		t.Fatal(item.Results)
	}
}

func TestHostKeyAlgorithmsAndRevocation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	algorithms := hostKeyAlgorithms(func(string, net.Addr, ssh.PublicKey) error {
		return &knownhosts.KeyError{Want: []knownhosts.KnownKey{{Key: pub}}}
	}, "127.0.0.1:22")
	if !slices.Equal(
		algorithms,
		[]string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA},
	) {
		t.Fatal(algorithms)
	}
	if hostKeyAlgorithms(
		func(string, net.Addr, ssh.PublicKey) error { return errors.New("no") },
		"127.0.0.1:22",
	) != nil {
		t.Fatal("a callback error other than a key mismatch returned algorithms")
	}

	f := newFixture(t)
	s := startServer(t, f.host, f.client.PublicKey(), behavior{stdout: probeJSON(t, allOK())})
	line := "@revoked " + knownhosts.Line(
		[]string{knownhosts.Normalize(s.address)},
		f.host.PublicKey(),
	) + "\n"
	if err := os.WriteFile(f.knownFile, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if item := probe(t, f, s.address); item.Results["ssh"] != "host_key_revoked" {
		t.Fatal(item.Results)
	}
}

func TestCollectionThroughRunnerLeaksNothing(t *testing.T) {
	f := newFixture(t)
	s := startServer(
		t,
		f.host,
		f.client.PublicKey(),
		behavior{stdout: []byte("token=hunter2 not json"), stderr: "secret-stderr", exit: 7},
	)
	f.pin(t, s.address, f.host.PublicKey())
	cfg := config.Hosts{
		User:           "argus",
		PrivateKeyFile: f.keyFile,
		KnownHostsFile: f.knownFile,
		TimeoutSeconds: 5,
		Concurrency:    2,
		Hosts: []config.Host{
			{Name: "bad", Address: s.address},
			{Name: "gone", Address: "127.0.0.1:1"},
		},
	}
	h := checktest.New(New(cfg, f.prober()), time.Time{}).Refresh(t)
	code, e := h.Get(t, Route)
	if code != 503 ||
		e.Items["bad"] != "script: exit_7; disk: unknown; inodes: unknown; memory: unknown; swap: unknown; load: unknown; cpu_pressure: unknown; uptime: unknown; failed_units: unknown; zfs: unknown; time_sync: unknown; ups: unknown" ||
		!strings.HasPrefix(e.Items["gone"], "ssh: unreachable") {
		t.Fatal(e.Items)
	}
	h.NoLeak(t, "hunter2", "secret-stderr")
}
