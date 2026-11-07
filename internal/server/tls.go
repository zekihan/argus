package server

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Reloader serves a certificate from files and re-reads them when they change,
// so a cert-manager renewal takes effect without a restart or Reloader.
type Reloader struct {
	certFile, keyFile string
	interval          time.Duration
	logger            *slog.Logger

	mu       sync.Mutex
	cert     *tls.Certificate
	certPEM  []byte
	keyPEM   []byte
	checked  time.Time
	failures int
}

// NewReloader loads the initial key pair; a broken pair at startup is fatal.
func NewReloader(certFile, keyFile string, interval time.Duration, logger *slog.Logger) (*Reloader, error) {
	r := &Reloader{certFile: certFile, keyFile: keyFile, interval: interval, logger: logger}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(data) > 1024*1024 {
		return nil, fmt.Errorf("file exceeds read budget")
	}
	return data, nil
}

// reload must be called with mu held, or before the Reloader is shared.
func (r *Reloader) reload() error {
	r.checked = time.Now()
	certPEM, err := readSmall(r.certFile)
	if err != nil {
		return fmt.Errorf("read TLS certificate file")
	}
	keyPEM, err := readSmall(r.keyFile)
	if err != nil {
		return fmt.Errorf("read TLS key file")
	}
	if r.cert != nil && bytes.Equal(certPEM, r.certPEM) && bytes.Equal(keyPEM, r.keyPEM) {
		return nil
	}
	// A renewal writes both files; a half-written pair fails to parse and the
	// previous certificate keeps serving until the next check.
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("TLS certificate and key do not form a valid pair")
	}
	r.cert, r.certPEM, r.keyPEM = &cert, certPEM, keyPEM
	if r.logger != nil {
		notAfter := ""
		if cert.Leaf != nil {
			notAfter = cert.Leaf.NotAfter.UTC().Format(time.RFC3339)
		}
		r.logger.Info("TLS certificate loaded", "notAfter", notAfter)
	}
	return nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if time.Since(r.checked) >= r.interval {
		if err := r.reload(); err != nil {
			r.failures++
			if r.logger != nil && r.failures == 1 {
				r.logger.Error("TLS certificate reload failed; serving the previous certificate")
			}
		} else {
			r.failures = 0
		}
	}
	return r.cert, nil
}
