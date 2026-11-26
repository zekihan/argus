// Package pbs reads Proxmox Backup Server datastore groups.
package pbs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/runner"
)

const (
	maxBody   = 1 << 20
	maxGroups = 5000
)

// Group is one backup group: store/type/id and the newest backup time.
type Group struct {
	Store   string
	Type    string
	ID      string
	Last    time.Time
	HasLast bool
	Guest   string
}

func (g Group) Key() string { return g.Store + "/" + g.Type + "/" + g.ID }

// API is the read-only PBS admin API.
type API struct {
	base   string
	token  string
	client *http.Client
}

// NewAPI builds a client that verifies the PBS certificate with the configured
// CA, fingerprint, or both. The token file is read on each request.
func NewAPI(c config.PBS) (*API, error) {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport is not *http.Transport")
	}
	transport := defaultTransport.Clone()
	transport.Proxy = nil
	tlsConfig, err := tlsConfig(c)
	if err != nil {
		return nil, err
	}
	transport.TLSClientConfig = tlsConfig
	return &API{
		base:  strings.TrimRight(c.URL, "/"),
		token: c.TokenFile,
		client: &http.Client{
			Transport: transport,
			Timeout:   0,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// pinnedVerifier verifies the leaf certificate against the fingerprint.
func pinnedVerifier(fingerprint []byte) func(raw [][]byte, verified [][]*x509.Certificate) error {
	return func(raw [][]byte, verified [][]*x509.Certificate) error {
		var leaf []byte
		if len(verified) > 0 && len(verified[0]) > 0 {
			leaf = verified[0][0].Raw
		} else if len(raw) > 0 {
			leaf = raw[0]
		}
		if leaf == nil || !fingerprintMatch(leaf, fingerprint) {
			return errTLS
		}
		cert, err := x509.ParseCertificate(leaf)
		if err != nil || time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
			return errTLS
		}
		return nil
	}
}

func tlsConfig(c config.PBS) (*tls.Config, error) {
	var fingerprint []byte
	if c.Fingerprint != "" {
		decoded, err := fingerprintBytes(c.Fingerprint)
		if err != nil {
			return nil, err
		}
		fingerprint = decoded
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, SessionTicketsDisabled: true}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, errors.New("read PBS CA file")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("PBS CA file contains no certificates")
		}
		cfg.RootCAs = pool
	}
	if fingerprint == nil {
		return cfg, nil
	}
	// A fingerprint identifies the leaf. PBS serves a certificate whose SAN
	// is the host name, so a client that dials the LAN address cannot rely on
	// hostname verification alone.
	if cfg.RootCAs == nil {
		cfg.InsecureSkipVerify = true
	}
	cfg.VerifyPeerCertificate = pinnedVerifier(fingerprint)
	return cfg, nil
}

func fingerprintMatch(leaf, want []byte) bool {
	sum := sha256.Sum256(leaf)
	if len(sum) != len(want) {
		return false
	}
	var diff byte
	for i := range want {
		diff |= sum[i] ^ want[i]
	}
	return diff == 0
}

func fingerprintBytes(s string) ([]byte, error) {
	s = strings.ToLower(strings.ReplaceAll(s, ":", ""))
	if len(s) != sha256.Size*2 {
		return nil, errors.New("fingerprint must be a SHA-256 digest")
	}
	return hex.DecodeString(s)
}

// Groups lists every backup group in every datastore.
func (a *API) Groups(ctx context.Context) ([]Group, error) {
	token, err := readToken(a.token)
	if err != nil {
		return nil, err
	}
	var stores []struct {
		Store string `json:"store"`
	}
	if err := a.get(ctx, token, "/api2/json/admin/datastore", &stores); err != nil {
		return nil, err
	}
	var out []Group
	for _, store := range stores {
		if !config.PBSName(store.Store) {
			return nil, runner.Fail("invalid_group")
		}
		var groups []struct {
			Type    string   `json:"backup-type"`
			ID      string   `json:"backup-id"`
			Last    *float64 `json:"last-backup"`
			Comment string   `json:"comment"`
		}
		path := "/api2/json/admin/datastore/" + url.PathEscape(store.Store) + "/groups"
		if err := a.get(ctx, token, path, &groups); err != nil {
			return nil, err
		}
		for _, group := range groups {
			if !config.PBSName(group.Type) || !config.PBSName(group.ID) {
				return nil, runner.Fail("invalid_group")
			}
			item := Group{Store: store.Store, Type: group.Type, ID: group.ID}
			if group.Last != nil && *group.Last > 0 {
				item.Last = time.Unix(int64(*group.Last), 0).UTC()
				item.HasLast = true
			}
			if config.PBSName(group.Comment) {
				item.Guest = group.Comment
			}
			out = append(out, item)
			if len(out) > maxGroups {
				return nil, runner.Fail("too_many")
			}
		}
	}
	return out, nil
}

func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", runner.Fail("token_unreadable")
	}
	token := strings.TrimRight(string(data), "\r\n")
	if !validToken(token) {
		return "", runner.Fail("token_invalid")
	}
	return token, nil
}

func validToken(s string) bool {
	if len(s) < 8 || len(s) > 512 || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	user, rest, ok := strings.Cut(s, "!")
	if !ok || !strings.Contains(user, "@") {
		return false
	}
	id, secret, ok := strings.Cut(rest, ":")
	return ok && id != "" && secret != ""
}

func (a *API) get(ctx context.Context, token, path string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return runner.Fail("request_failed")
	}
	req.Header.Set("Authorization", "PBSAPIToken="+token)
	resp, err := a.client.Do(req)
	if err != nil {
		if errors.Is(err, errTLS) {
			return runner.Fail("tls")
		}
		return runner.Fail("request_failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		return runner.Fail("decode")
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return runner.Fail("unauthorized")
	case http.StatusForbidden:
		return runner.Fail("forbidden")
	default:
		return runner.Fail("http")
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil ||
		len(envelope.Data) == 0 || envelope.Data[0] != '[' {
		return runner.Fail("decode")
	}
	if err := json.Unmarshal(envelope.Data, dest); err != nil {
		return runner.Fail("decode")
	}
	return nil
}

// errTLS is the only certificate error returned to the transport. Its text is
// fixed so a handshake failure cannot echo the request or the token.
var errTLS = errors.New("tls")
