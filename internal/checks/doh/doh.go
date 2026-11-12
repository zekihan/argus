// Package doh checks a positive DNS answer over the configured HTTPS route.
package doh

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/result"
)

type Check struct {
	config config.DoH
	client *http.Client
}

func New(c config.DoH) *Check {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if c.DNSResolver != "" {
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, c.DNSResolver)
		}}
		transport.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Resolver: resolver}).DialContext
	}
	return &Check{c, &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (*Check) Name() string { return "doh" }
func (*Check) Describe() []result.Collection {
	return []result.Collection{{Route: "doh", Check: "doh", Singleton: true, Dimensions: []string{"answer"}}}
}
func (c *Check) Collect(ctx context.Context, _ func() time.Time) ([]result.Collection, error) {
	verdict := "invalid_response"
	query := question(c.config.Name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.URL, bytes.NewReader(query))
	if err == nil {
		req.Header.Set("Content-Type", "application/dns-message")
		req.Header.Set("Accept", "application/dns-message")
		resp, e := c.client.Do(req)
		if e == nil {
			defer func() { _ = resp.Body.Close() }()
			media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			body, e := io.ReadAll(io.LimitReader(resp.Body, 65536))
			if e == nil && len(body) <= 65535 && resp.StatusCode == http.StatusOK && media == "application/dns-message" && valid(body, query, c.config.Name, net.ParseIP(c.config.ExpectedIPv4).To4()) {
				verdict = result.OK
			}
		}
	}
	return []result.Collection{{Route: "doh", Check: "doh", Singleton: true, Dimensions: []string{"answer"}, Results: map[string]string{"answer": verdict}}}, nil
}
func question(name string) []byte {
	b := []byte{0, 0, 1, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		b = append(b, byte(len(label)))
		b = append(b, label...)
	}
	return append(b, 0, 0, 1, 0, 1)
}

// readName follows bounded compression pointers and returns the offset after the encoded name.
func readName(b []byte, off int) (string, int, bool) {
	var labels []string
	next := -1
	steps := 0
	length := 0
	for {
		if off >= len(b) || steps > 127 {
			return "", 0, false
		}
		steps++
		n := int(b[off])
		off++
		if n&0xc0 == 0xc0 {
			if off >= len(b) {
				return "", 0, false
			}
			ptr := ((n & 0x3f) << 8) | int(b[off])
			off++
			if ptr >= off-2 {
				return "", 0, false
			}
			if next < 0 {
				next = off
			}
			off = ptr
			continue
		}
		if n&0xc0 != 0 || n > 63 || off+n > len(b) {
			return "", 0, false
		}
		if n == 0 {
			if next < 0 {
				next = off
			}
			return strings.ToLower(strings.Join(labels, ".")), next, true
		}
		length += n + 1
		if length > 254 {
			return "", 0, false
		}
		labels = append(labels, string(b[off:off+n]))
		off += n
	}
}
func valid(b, q []byte, name string, expected net.IP) bool {
	if len(b) < 12 || len(q) < 12 || binary.BigEndian.Uint16(b[:2]) != binary.BigEndian.Uint16(q[:2]) || b[2]&0xfa != 0x80 || b[3]&0x0f != 0 || binary.BigEndian.Uint16(b[4:6]) != 1 {
		return false
	}
	an := int(binary.BigEndian.Uint16(b[6:8]))
	ns := int(binary.BigEndian.Uint16(b[8:10]))
	ar := int(binary.BigEndian.Uint16(b[10:12]))
	if an == 0 {
		return false
	}
	qname, off, ok := readName(b, 12)
	if !ok || qname != strings.ToLower(strings.TrimSuffix(name, ".")) || off+4 > len(b) || binary.BigEndian.Uint16(b[off:off+2]) != 1 || binary.BigEndian.Uint16(b[off+2:off+4]) != 1 {
		return false
	}
	off += 4
	found := false
	for i := 0; i < an+ns+ar; i++ {
		owner, n, good := readName(b, off)
		if !good || n+10 > len(b) {
			return false
		}
		typ := binary.BigEndian.Uint16(b[n : n+2])
		class := binary.BigEndian.Uint16(b[n+2 : n+4])
		size := int(binary.BigEndian.Uint16(b[n+8 : n+10]))
		off = n + 10
		if off+size > len(b) {
			return false
		}
		if i < an && owner == qname && typ == 1 && class == 1 && size == 4 && net.IP(b[off:off+4]).Equal(expected) {
			found = true
		}
		off += size
	}
	return found && off == len(b)
}
