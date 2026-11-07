// Package kube is a small hand-written Kubernetes API client: paginated
// lists, single-object GET, ConfigMap get/update and nothing else.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Error codes. They are the only diagnostics a Kubernetes failure produces.
const (
	CodeForbidden    = "forbidden"
	CodeUnauthorized = "unauthorized"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeHTTP         = "api_error"
	CodeUnreachable  = "unreachable"
	CodeTimeout      = "timeout"
	CodeInvalid      = "invalid_response"
	CodeBudget       = "over_budget"
	CodeToken        = "token_unavailable"
	CodeClient       = "client_error"
)

// Error contains only client-generated diagnostics, never response bodies or credentials.
type Error struct {
	Code   string
	Status int
	Path   string
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("kubernetes %s (HTTP %d) for %s", e.Code, e.Status, e.Path)
	}
	return fmt.Sprintf("kubernetes %s for %s", e.Code, e.Path)
}

// CodeOf maps any error to a fixed code without exposing its text.
func CodeOf(err error) string {
	var kerr *Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &kerr):
		return kerr.Code
	case errors.Is(err, context.DeadlineExceeded):
		return CodeTimeout
	default:
		return CodeClient
	}
}

// IsConflict reports a failed precondition (HTTP 409).
func IsConflict(err error) bool { return CodeOf(err) == CodeConflict }

// IsNotFound reports HTTP 404.
func IsNotFound(err error) bool { return CodeOf(err) == CodeNotFound }

type Client struct {
	BaseURL   string
	TokenFile string
	HTTP      *http.Client
	UserAgent string

	mu          sync.Mutex
	lastSuccess time.Time
	lastFailure string
}

// Loopback reports whether origin is a plain-HTTP loopback origin such as a
// local `kubectl proxy`. Only such an origin may skip TLS and the token.
func Loopback(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.Port() == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// New returns a client for an HTTPS API origin with a projected service-account
// token and CA bundle, or for a plain-HTTP loopback origin (kubectl proxy),
// which carries no credentials.
func New(base, tokenFile, caFile, version string, timeout time.Duration) (*Client, error) {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout, IdleConnTimeout: 90 * time.Second,
		MaxIdleConnsPerHost: 4, MaxConnsPerHost: 4}
	client := &Client{BaseURL: strings.TrimRight(base, "/"), UserAgent: "argus/" + version,
		HTTP: &http.Client{Transport: transport, Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if Loopback(base) {
		return client, nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("kubernetes API URL must be an HTTPS origin or a plain-HTTP loopback origin")
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read Kubernetes CA file")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("kubernetes CA file contains no certificates")
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	client.TokenFile = tokenFile
	return client, nil
}

// Health returns the time of the last successful API response and the code of
// the most recent failure.
func (c *Client) Health() (time.Time, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastSuccess, c.lastFailure
}

func (c *Client) record(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.lastSuccess = time.Now()
		return
	}
	var kerr *Error
	// A canceled collection says nothing about the API server.
	if errors.As(err, &kerr) && kerr.Code != CodeClient {
		c.lastFailure = kerr.Code
	}
}

// List returns every item of a paginated list from one consistent snapshot.
func List[T any](ctx context.Context, c *Client, path, selector string) ([]T, error) {
	query := url.Values{"limit": {"200"}}
	if selector != "" {
		query.Set("labelSelector", selector)
	}
	var items []T
	seen := map[string]bool{}
	resourceVersion := ""
	for range 100 {
		data, err := c.page(ctx, path, query)
		if err != nil {
			return nil, err
		}
		var page struct {
			Metadata *struct {
				Continue        string `json:"continue"`
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
			Items *[]T `json:"items"`
		}
		if json.Unmarshal(data, &page) != nil || page.Metadata == nil || page.Items == nil {
			return nil, &Error{Code: CodeInvalid, Path: path}
		}
		if resourceVersion != "" && resourceVersion != page.Metadata.ResourceVersion {
			return nil, &Error{Code: CodeInvalid, Path: path}
		}
		resourceVersion = page.Metadata.ResourceVersion
		items = append(items, (*page.Items)...)
		if len(items) > 20000 {
			return nil, &Error{Code: CodeBudget, Path: path}
		}
		next := page.Metadata.Continue
		if next == "" {
			return items, nil
		}
		if seen[next] {
			return nil, &Error{Code: CodeInvalid, Path: path}
		}
		seen[next] = true
		query.Set("continue", next)
	}
	return nil, &Error{Code: CodeBudget, Path: path}
}

// Get decodes one object.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	data, err := c.page(ctx, path, nil)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, out) != nil {
		return &Error{Code: CodeInvalid, Path: path}
	}
	return nil
}

func (c *Client) page(ctx context.Context, path string, query url.Values) ([]byte, error) {
	for attempt := range 3 {
		data, retry, err := c.request(ctx, http.MethodGet, path, query, nil)
		if err == nil || !retry || attempt == 2 {
			return data, err
		}
		delay := (100 * time.Millisecond << attempt) + time.Duration(rand.IntN(100))*time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, &Error{Code: CodeTimeout, Path: path}
		case <-timer.C:
		}
	}
	return nil, &Error{Code: CodeUnreachable, Path: path}
}

func (c *Client) request(ctx context.Context, method, path string, query url.Values, body []byte) (data []byte, retry bool, err error) {
	defer func() { c.record(err) }()
	target := c.BaseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, false, &Error{Code: CodeClient, Path: path}
	}
	if c.TokenFile != "" {
		// Projected service-account tokens rotate independently of the process.
		token, err := os.ReadFile(c.TokenFile)
		if err != nil || strings.TrimSpace(string(token)) == "" {
			return nil, false, &Error{Code: CodeToken, Path: path}
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, &Error{Code: CodeTimeout, Path: path}
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, true, &Error{Code: CodeTimeout, Path: path}
		}
		return nil, true, &Error{Code: CodeUnreachable, Path: path}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		retry := response.StatusCode == 429 || response.StatusCode >= 500 || response.StatusCode == 401
		code := CodeHTTP
		switch response.StatusCode {
		case http.StatusForbidden:
			code = CodeForbidden
		case http.StatusUnauthorized:
			code = CodeUnauthorized
		case http.StatusNotFound:
			code = CodeNotFound
		case http.StatusConflict:
			code = CodeConflict
		}
		return nil, retry, &Error{Code: code, Status: response.StatusCode, Path: path}
	}
	data, err = io.ReadAll(io.LimitReader(response.Body, 16*1024*1024+1))
	if ctx.Err() != nil {
		return nil, false, &Error{Code: CodeTimeout, Path: path}
	}
	if len(data) > 16*1024*1024 {
		return nil, false, &Error{Code: CodeBudget, Path: path}
	}
	if err != nil {
		return nil, ctx.Err() == nil, &Error{Code: CodeUnreachable, Path: path}
	}
	return data, false, nil
}

// ConfigMap is a ConfigMap read for a conditional update. Every field other
// than data is sent back unchanged, so labels and annotations survive a PUT.
type ConfigMap struct {
	Data            map[string]string
	ResourceVersion string
	raw             map[string]json.RawMessage
}

func configMapPath(namespace, name string) string {
	return "/api/v1/namespaces/" + url.PathEscape(namespace) + "/configmaps/" + url.PathEscape(name)
}

func decodeConfigMap(path string, data []byte) (ConfigMap, error) {
	var raw map[string]json.RawMessage
	var view struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if json.Unmarshal(data, &raw) != nil || json.Unmarshal(data, &view) != nil || view.Metadata.ResourceVersion == "" {
		return ConfigMap{}, &Error{Code: CodeInvalid, Path: path}
	}
	return ConfigMap{Data: view.Data, ResourceVersion: view.Metadata.ResourceVersion, raw: raw}, nil
}

// GetConfigMap reads one ConfigMap with its resourceVersion.
func (c *Client) GetConfigMap(ctx context.Context, namespace, name string) (ConfigMap, error) {
	path := configMapPath(namespace, name)
	data, err := c.page(ctx, path, nil)
	if err != nil {
		return ConfigMap{}, err
	}
	return decodeConfigMap(path, data)
}

// UpdateConfigMap replaces data with a PUT conditional on the resourceVersion
// read by GetConfigMap. A concurrent change returns a conflict error. It is
// never retried here: callers re-read and converge.
func (c *Client) UpdateConfigMap(ctx context.Context, namespace, name string, current ConfigMap, data map[string]string) (ConfigMap, error) {
	path := configMapPath(namespace, name)
	if current.raw == nil || current.ResourceVersion == "" {
		return ConfigMap{}, &Error{Code: CodeClient, Path: path}
	}
	object := make(map[string]json.RawMessage, len(current.raw))
	for key, value := range current.raw {
		object[key] = value
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return ConfigMap{}, &Error{Code: CodeClient, Path: path}
	}
	object["data"] = encoded
	body, err := json.Marshal(object)
	if err != nil {
		return ConfigMap{}, &Error{Code: CodeClient, Path: path}
	}
	response, _, err := c.request(ctx, http.MethodPut, path, nil, body)
	if err != nil {
		return ConfigMap{}, err
	}
	return decodeConfigMap(path, response)
}
