package doh

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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
		{"servfail", edit(func(b []byte) []byte { b[3] = 0x82; return b }), 200, "application/dns-message", false},
		{"nxdomain", edit(func(b []byte) []byte { b[3] = 0x83; return b }), 200, "application/dns-message", false},
		{"empty answer", edit(func(b []byte) []byte { b = b[:len(b)-16]; b[7] = 0; return b }), 200, "application/dns-message", false},
		{"empty body", nil, 200, "application/dns-message", false},
		{"truncated record", good[:len(good)-1], 200, "application/dns-message", false},
		{"trailing bytes", append(append([]byte(nil), good...), 0), 200, "application/dns-message", false},
		{"id mismatch", edit(func(b []byte) []byte { b[1] = 1; return b }), 200, "application/dns-message", false},
		{"query not response", edit(func(b []byte) []byte { b[2] = 0x01; return b }), 200, "application/dns-message", false},
		{"truncated flag", edit(func(b []byte) []byte { b[2] = 0x83; return b }), 200, "application/dns-message", false},
		{"opcode", edit(func(b []byte) []byte { b[2] = 0x89; return b }), 200, "application/dns-message", false},
		{"wrong question", edit(func(b []byte) []byte { b[13] = 'x'; return b }), 200, "application/dns-message", false},
		{"wrong answer", edit(func(b []byte) []byte { b[len(b)-1] = 2; return b }), 200, "application/dns-message", false},
		{"pointer loop", edit(func(b []byte) []byte { b[33], b[34] = 0xc0, 33; return b }), 200, "application/dns-message", false},
		{"forward pointer", edit(func(b []byte) []byte { b[33], b[34] = 0xc0, 40; return b }), 200, "application/dns-message", false},
		{"dns body as html", good, 200, "text/html", false},
		{"dns body without type", good, 200, "", false},
		{"login page", []byte("<html>login</html>"), 200, "text/html", false},
		{"redirect to a valid answer", nil, 302, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/dns-query", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" || r.Header.Get("Accept") != "application/dns-message" {
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
			c := New(config.DoH{URL: server.URL + "/dns-query", Name: "one.one.one.one", ExpectedIPv4: "1.1.1.1"})
			c.client.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig
			got, err := c.Collect(context.Background(), time.Now)
			if err != nil || (got[0].Results["answer"] == result.OK) != tc.want {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}

func TestProbeDetectsResolverFailureAfterSuccess(t *testing.T) {
	dns, err := net.ListenPacket("udp4", "127.0.0.1:0")
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

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(noerror())
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c := New(config.DoH{
		URL:          "https://" + net.JoinHostPort("probe.example.com.", port) + "/dns-query",
		Name:         "one.one.one.one",
		ExpectedIPv4: "1.1.1.1",
		DNSResolver:  dns.LocalAddr().String(),
	})
	c.client.Transport.(*http.Transport).TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig

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
			t.Fatalf("resolver rejects queries: %t, got %v, %v, want %s", tc.reject, got, err, tc.want)
		}
	}
}
