package hosts

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
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
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

type behaviour struct {
	stdout []byte
	stderr string
	exit   uint32
	hang   bool
}

type server struct {
	address  string
	mu       sync.Mutex
	requests []string
}

func newSigner(t *testing.T) ssh.Signer {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// startServer is an SSH server fixture that authorises one client key and
// answers a session the way a forced command would.
func startServer(t *testing.T, host ssh.Signer, authorized ssh.PublicKey, b behaviour) *server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	s := &server{address: listener.Addr().String()}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) == string(authorized.Marshal()) {
			return &ssh.Permissions{}, nil
		}
		return nil, os.ErrPermission
	}}
	cfg.AddHostKey(host)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for newChannel := range channels {
					channel, reqs, err := newChannel.Accept()
					if err != nil {
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
						_ = req.Reply(true, nil)
						if b.hang {
							time.Sleep(2 * time.Second)
						}
						_, _ = channel.Write(b.stdout)
						_, _ = channel.Stderr().Write([]byte(b.stderr))
						status := make([]byte, 4)
						binary.BigEndian.PutUint32(status, b.exit)
						_, _ = channel.SendRequest("exit-status", false, status)
						_ = channel.Close()
					}
				}
			}()
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
	dir := t.TempDir()
	f := &fixture{dir: dir, client: newSigner(t), host: newSigner(t), keyFile: filepath.Join(dir, "id"), knownFile: filepath.Join(dir, "known_hosts")}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.keyFile, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	f.client, err = ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.knownFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) pin(t *testing.T, address string, key ssh.PublicKey) {
	line := knownhosts.Line([]string{knownhosts.Normalize(address)}, key) + "\n"
	file, err := os.OpenFile(f.knownFile, os.O_APPEND|os.O_WRONLY, 0600)
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
	s := startServer(t, f.host, f.client.PublicKey(), behaviour{stdout: probeJSON(t, allOK())})
	if item := probe(t, f, s.address); item.Results["ssh"] != "host_key_unknown" || item.Results["disk"] != "unknown" {
		t.Fatal("unknown host key accepted", item.Results)
	}
	f.pin(t, s.address, newSigner(t).PublicKey())
	if item := probe(t, f, s.address); item.Results["ssh"] != "host_key_mismatch" {
		t.Fatal("changed host key accepted", item.Results)
	}
	if err := os.WriteFile(f.knownFile, nil, 0600); err != nil {
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
	s := startServer(t, f.host, newSigner(t).PublicKey(), behaviour{stdout: probeJSON(t, allOK())})
	f.pin(t, s.address, f.host.PublicKey())
	if item := probe(t, f, s.address); item.Results["ssh"] != "auth_failed" {
		t.Fatal(item.Results)
	}
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	address := closed.Addr().String()
	closed.Close()
	f.pin(t, address, f.host.PublicKey())
	if item := probe(t, f, address); item.Results["ssh"] != "unreachable" {
		t.Fatal(item.Results)
	}
	if err := os.WriteFile(f.keyFile, []byte("not a key"), 0600); err != nil {
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
		b    behaviour
		dim  string
		want string
	}{
		{"exit 1 with results", behaviour{stdout: probeJSON(t, failing), exit: 1}, "disk", `/a"b\c=95% /other=90% (threshold: 90%)`},
		{"missing tool", behaviour{stdout: probeJSON(t, failing), exit: 1}, "zfs", "n/a"},
		{"failing tool", behaviour{stdout: probeJSON(t, failing), exit: 1}, "ups", "unavailable"},
		{"exit 3", behaviour{stdout: probeJSON(t, allOK()), exit: 3, stderr: "sudo: password=hunter2"}, "script", "exit_3"},
		{"invalid output", behaviour{stdout: []byte("sudo: a password is required\n"), exit: 1}, "script", "invalid_output"},
		{"oversized output", behaviour{stdout: []byte(strings.Repeat("x", maxOutput+1))}, "script", "output_too_large"},
		{"timeout", behaviour{hang: true, stdout: probeJSON(t, allOK())}, "script", "timeout"},
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

func TestExpectedDimensionsTurnMissingToolsUnavailable(t *testing.T) {
	values := allOK()
	values["zfs"], values["ups"] = "n/a", "n/a"
	item := Evaluate(config.Host{Name: "pve", Expects: []string{"zfs"}}, Outcome{SSH: "ok", Exit: 0, Output: probeJSON(t, values)})
	if item.Results["zfs"] != "unavailable" || item.Results["ups"] != "n/a" {
		t.Fatal(item.Results)
	}
	delete(values, "swap")
	item = Evaluate(config.Host{Name: "pve"}, Outcome{SSH: "ok", Exit: 0, Output: probeJSON(t, values)})
	if item.Results["swap"] != "unknown" {
		t.Fatal("missing probe field passed", item.Results)
	}
	item = Evaluate(config.Host{Name: "pve"}, Outcome{SSH: "ok", Exit: 0, Output: []byte(`{"disk":1}`)})
	if item.Results["disk"] != "unknown" {
		t.Fatal(item.Results)
	}
	long := allOK()
	long["failed_units"] = strings.Repeat("unit.service ", 100)
	item = Evaluate(config.Host{Name: "pve"}, Outcome{SSH: "ok", Exit: 1, Output: probeJSON(t, long)})
	if len(item.Results["failed_units"]) > maxFieldLength {
		t.Fatal("probe field not capped")
	}
}

func TestCollectionThroughRunnerLeaksNothing(t *testing.T) {
	f := newFixture(t)
	s := startServer(t, f.host, f.client.PublicKey(), behaviour{stdout: []byte("token=hunter2 not json"), stderr: "secret-stderr", exit: 7})
	f.pin(t, s.address, f.host.PublicKey())
	cfg := config.Hosts{User: "argus", PrivateKeyFile: f.keyFile, KnownHostsFile: f.knownFile, TimeoutSeconds: 5, Concurrency: 2,
		Hosts: []config.Host{{Name: "bad", Address: s.address}, {Name: "gone", Address: "127.0.0.1:1"}}}
	h := checktest.New(New(cfg, f.prober()), time.Time{}).Refresh(t)
	code, e := h.Get(t, Route)
	if code != 503 || e.Items["bad"] != "script: exit_7; disk: unknown; inodes: unknown; memory: unknown; swap: unknown; load: unknown; cpu_pressure: unknown; uptime: unknown; failed_units: unknown; zfs: unknown; time_sync: unknown; ups: unknown" ||
		!strings.HasPrefix(e.Items["gone"], "ssh: unreachable") {
		t.Fatal(e.Items)
	}
	h.NoLeak(t, "hunter2", "secret-stderr")
}
