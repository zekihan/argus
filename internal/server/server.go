// Package server serves the versioned HTTP/JSON API: bearer authentication on
// every route except /healthz, and native TLS that follows certificate renewals.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/zekihan/argus/internal/runner"
)

// Routes is what the handler serves.
type Routes interface {
	Lookup(path string, now time.Time) (int, any)
	Index(now time.Time) (int, runner.Index)
}

func write(w http.ResponseWriter, r *http.Request, code int, payload any) {
	w.WriteHeader(code)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(payload)
	}
}

func Handler(routes Routes, tokenFile string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" {
			// Authenticate before routing so unknown paths reveal nothing.
			if code := authorize(r, tokenFile); code != http.StatusOK {
				if code == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer realm="argus"`)
				}
				write(w, r, code, map[string]string{"reason": "access denied"})
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			write(w, r, http.StatusMethodNotAllowed, map[string]string{"reason": "method not allowed"})
			return
		}
		now := time.Now()
		switch path := r.URL.Path; {
		case path == "/healthz":
			write(w, r, http.StatusOK, map[string]bool{"healthy": true})
		case path == "/v1/checks":
			code, index := routes.Index(now)
			write(w, r, code, index)
		case strings.HasPrefix(path, "/v1/") && len(path) > len("/v1/") && !strings.HasSuffix(path, "/"):
			code, payload := routes.Lookup(strings.TrimPrefix(path, "/v1/"), now)
			write(w, r, code, payload)
		default:
			write(w, r, http.StatusNotFound, map[string]string{"reason": "unknown route"})
		}
	})
}

// Serve runs the poller and the HTTP server, and drains both on cancellation.
// With certificates it serves TLS only, re-reading the files as they change.
func Serve(ctx context.Context, listener net.Listener, run func(context.Context), routes Routes, tokenFile string, certificates *Reloader, logger *slog.Logger) error {
	return serve(ctx, listener, run, routes, tokenFile, certificates, logger, 15*time.Second)
}

func serve(ctx context.Context, listener net.Listener, run func(context.Context), routes Routes, tokenFile string, certificates *Reloader, logger *slog.Logger, shutdownTimeout time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{Handler: Handler(routes, tokenFile), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024,
		// TLS handshake failures from scanners are not actionable and may echo client data.
		ErrorLog: slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)}
	if certificates != nil {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certificates.GetCertificate}
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); run(ctx) }()
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	var err error
	select {
	case <-ctx.Done():
	case err = <-serveDone:
	}
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer drainCancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	} else if err != nil {
		// Never surface the network error text.
		err = errors.New("HTTP server failed")
	}
	if shutdownErr := server.Shutdown(drainCtx); shutdownErr != nil {
		_ = server.Close()
		err = errors.Join(err, shutdownErr)
	}
	select {
	case <-pollDone:
	case <-drainCtx.Done():
		err = errors.Join(err, fmt.Errorf("wait for polling goroutine: %w", drainCtx.Err()))
	}
	if logger != nil {
		logger.Info("Server stopped")
	}
	return err
}
