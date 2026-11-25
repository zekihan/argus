package doh

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/checks/checktest"
	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/result"
)

// noerror is a recorded NOERROR reply for one.one.one.one A with answer 1.1.1.1.
func noerror() []byte {
	b := question("one.one.one.one")
	b[2] = 0x81
	b[3] = 0x80
	binary.BigEndian.PutUint16(b[6:8], 1)
	return append(b, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 1, 1, 1, 1)
}

func edit(f func([]byte) []byte) []byte { return f(noerror()) }

func TestWireResponses(t *testing.T) {
	good := noerror()
	cases := []struct {
		name    string
		body    []byte
		status  int
		content string
		want    bool
	}{
		{"noerror", good, 200, "application/dns-message", true},
		{"noerror with parameters", good, 200, "application/dns-message; charset=binary", true},
		{
			"servfail",
			edit(func(b []byte) []byte { b[3] = 0x82; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"nxdomain",
			edit(func(b []byte) []byte { b[3] = 0x83; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"empty answer",
			edit(func(b []byte) []byte { b = b[:len(b)-16]; b[7] = 0; return b }),
			200,
			"application/dns-message",
			false,
		},
		{"empty body", nil, 200, "application/dns-message", false},
		{"truncated record", good[:len(good)-1], 200, "application/dns-message", false},
		{
			"trailing bytes",
			append(append([]byte(nil), good...), 0),
			200,
			"application/dns-message",
			false,
		},
		{
			"id mismatch",
			edit(func(b []byte) []byte { b[1] = 1; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"query not response",
			edit(func(b []byte) []byte { b[2] = 0x01; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"truncated flag",
			edit(func(b []byte) []byte { b[2] = 0x83; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"opcode",
			edit(func(b []byte) []byte { b[2] = 0x89; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"wrong question",
			edit(func(b []byte) []byte { b[13] = 'x'; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"wrong answer",
			edit(func(b []byte) []byte { b[len(b)-1] = 2; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"pointer loop",
			edit(func(b []byte) []byte { b[33], b[34] = 0xc0, 33; return b }),
			200,
			"application/dns-message",
			false,
		},
		{
			"forward pointer",
			edit(func(b []byte) []byte { b[33], b[34] = 0xc0, 40; return b }),
			200,
			"application/dns-message",
			false,
		},
		{"dns body as html", good, 200, "text/html", false},
		{"dns body without type", good, 200, "", false},
		{"login page", []byte("<html>login</html>"), 200, "text/html", false},
		{"redirect to a valid answer", nil, 302, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/dns-query", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost ||
					r.Header.Get("Content-Type") != "application/dns-message" ||
					r.Header.Get("Accept") != "application/dns-message" {
					t.Error("invalid request")
				}
				if tc.status == http.StatusFound {
					http.Redirect(w, r, "/valid", http.StatusFound)
					return
				}
				if tc.content != "" {
					w.Header().Set("Content-Type", tc.content)
				} else {
					w.Header()["Content-Type"] = nil
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write(tc.body)
			})
			mux.HandleFunc("/valid", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/dns-message")
				_, _ = w.Write(good)
			})
			server := httptest.NewTLSServer(mux)
			defer server.Close()
			// Exercise the production client from New; only trust the test certificate.
			c := New(
				config.DoH{
					URL:          server.URL + "/dns-query",
					Name:         "one.one.one.one",
					ExpectedIPv4: "1.1.1.1",
				},
			)
			serverTransport, ok := server.Client().Transport.(*http.Transport)
			if !ok {
				t.Fatal("test server transport is not *http.Transport")
			}
			clientTransport, ok := c.client.Transport.(*http.Transport)
			if !ok {
				t.Fatal("check client transport is not *http.Transport")
			}
			clientTransport.TLSClientConfig = serverTransport.TLSClientConfig
			got, err := c.Collect(t.Context(), time.Now)
			if err != nil || (got[0].Results["answer"] == result.OK) != tc.want {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

func TestRejectedAnswersAndStaleSamplesFailClosed(t *testing.T) {
	const secret = "<html>secret-login</html>" //nolint:gosec // Synthetic test marker, not a credential.
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(secret))
	}))
	defer server.Close()
	c := New(
		config.DoH{
			URL:          server.URL + "/dns-query",
			Name:         "one.one.one.one",
			ExpectedIPv4: "1.1.1.1",
		},
	)
	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("test server transport is not *http.Transport")
	}
	clientTransport, ok := c.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("check client transport is not *http.Transport")
	}
	clientTransport.TLSClientConfig = serverTransport.TLSClientConfig
	if c.Name() != "doh" || c.Describe()[0].Route != "doh" || c.Describe()[0].Singleton != true {
		t.Fatal(c.Describe())
	}
	got, err := c.Collect(t.Context(), time.Now)
	if err != nil || got[0].Results["answer"] != "invalid_response" {
		t.Fatal(got, err)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || strings.Contains(string(data), "secret-login") {
		t.Fatal(string(data))
	}
	h := checktest.New(c, time.Time{})
	// The harness wraps Collect, so point that client at the test server too.
	h.Refresh(t)
	code, body := h.Runner.Lookup("doh", time.Now().Add(400*24*time.Hour))
	envelope, ok := body.(result.Envelope)
	if !ok || code != 503 || !strings.HasPrefix(envelope.Results["collection"], "stale(") {
		t.Fatalf("stale sample = %d %+v", code, body)
	}
	h.NoLeak(t, "secret-login")
}

func TestReadNameRejectsMalformedCompression(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    []byte
	}{
		{"truncated", []byte{1}},
		{"truncated pointer", []byte{0xc0}},
		{"forward pointer", []byte{0xc0, 0x02, 0}},
		{"reserved bits", []byte{0x40, 0}},
		{"label overrun", []byte{3, 'a'}},
		{"name too long", append(bytes.Repeat([]byte{63}, 1), bytes.Repeat([]byte{'a'}, 63)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := tc.b
			if tc.name == "name too long" {
				message = nil
				for range 5 {
					message = append(message, 63)
					message = append(message, bytes.Repeat([]byte{'a'}, 63)...)
				}
			}
			if _, _, ok := readName(message, 0); ok {
				t.Fatal("malformed name accepted")
			}
		})
	}
}

// localServer serves DNS responses over TLS with a loopback A record.
func localServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(noerror())
	}))
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return server, port
}

func TestProbeDetectsResolverFailureAfterSuccess(t *testing.T) {
	dns, err := (&net.ListenConfig{}).ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var reject atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		var packet [512]byte
		for {
			n, client, err := dns.ReadFrom(packet[:])
			if err != nil {
				return
			}
			_, off, ok := readName(packet[:n], 12)
			if !ok || off+4 > n {
				t.Error("invalid resolver question")
				continue
			}
			reply := append([]byte(nil), packet[:off+4]...)
			reply[2], reply[3] = 0x81, 0x80
			// Keep only the question, omitting any EDNS request data.
			clear(reply[6:12])
			if reject.Load() {
				reply[3] = 0x83 // NXDOMAIN
			} else if binary.BigEndian.Uint16(reply[off:off+2]) == 1 {
				reply[7] = 1 // One A record for the loopback HTTPS server.
				reply = append(reply, 0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 127, 0, 0, 1)
			}
			if _, err := dns.WriteTo(reply, client); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = dns.Close()
		<-done
	})

	server, port := localServer(t)
	c := New(config.DoH{
		URL:          "https://" + net.JoinHostPort("probe.example.com.", port) + "/dns-query",
		Name:         "one.one.one.one",
		ExpectedIPv4: "1.1.1.1",
		DNSResolver:  dns.LocalAddr().String(),
	})
	serverTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("test server transport is not *http.Transport")
	}
	clientTransport, ok := c.client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("check client transport is not *http.Transport")
	}
	clientTransport.TLSClientConfig = serverTransport.TLSClientConfig

	for _, tc := range []struct {
		reject bool
		want   string
	}{
		{false, result.OK},
		{true, "invalid_response"},
		{false, result.OK},
	} {
		reject.Store(tc.reject)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		got, err := c.Collect(ctx, time.Now)
		cancel()
		if err != nil || got[0].Results["answer"] != tc.want {
			t.Fatalf(
				"resolver rejects queries: %t, got %v, %v, want %s",
				tc.reject,
				got,
				err,
				tc.want,
			)
		}
	}
}
