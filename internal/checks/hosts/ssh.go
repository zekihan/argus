package hosts

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/result"
)

const maxOutput = 64 * 1024

// SSH runs the probe through a key whose authorized_keys entry forces the
// probe command. The key and known_hosts are re-read on every probe, so
// rotations take effect without a restart.
type SSH struct {
	User           string
	PrivateKeyFile string
	KnownHostsFile string
}

func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("file exceeds read budget")
	}
	return data, nil
}

// limited keeps at most maxOutput bytes and records whether more arrived.
type limited struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (l *limited) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	room := maxOutput - l.buf.Len()
	if len(p) > room {
		l.truncated = true
		l.buf.Write(p[:max(room, 0)])
		return len(p), nil
	}
	l.buf.Write(p)
	return len(p), nil
}

// hostKeyAlgorithms lists the algorithms of the pinned keys for address, so
// the server presents a key type known_hosts can verify.
func hostKeyAlgorithms(callback ssh.HostKeyCallback, address string) []string {
	err := callback(address, &net.TCPAddr{IP: net.IPv4zero}, probeKey{})
	var keyErr *knownhosts.KeyError
	if !errors.As(err, &keyErr) {
		return nil
	}
	var algorithms []string
	seen := map[string]bool{}
	for _, known := range keyErr.Want {
		kind := known.Key.Type()
		candidates := []string{kind}
		if kind == ssh.KeyAlgoRSA {
			candidates = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
		}
		for _, algorithm := range candidates {
			if !seen[algorithm] {
				seen[algorithm] = true
				algorithms = append(algorithms, algorithm)
			}
		}
	}
	return algorithms
}

// probeKey never matches a pinned key; it only elicits the list of known keys.
type probeKey struct{}

func (probeKey) Type() string                        { return "argus-host-key-lookup" }
func (probeKey) Marshal() []byte                     { return []byte("argus-host-key-lookup") }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("not a key") }

func (s SSH) Probe(ctx context.Context, host config.Host) Outcome {
	keyData, err := readSmall(s.PrivateKeyFile, 64*1024)
	if err != nil {
		return Outcome{SSH: result.Reason("key_unavailable"), Exit: -1}
	}
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return Outcome{SSH: result.Reason("key_unavailable"), Exit: -1}
	}
	known, err := knownhosts.New(s.KnownHostsFile)
	if err != nil {
		return Outcome{SSH: result.Reason("known_hosts_unavailable"), Exit: -1}
	}
	address, err := config.HostAddress(host.Address)
	if err != nil {
		return Outcome{SSH: result.Reason("unreachable"), Exit: -1}
	}
	var keyFailure string
	callback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := known(hostname, remote, key)
		var keyErr *knownhosts.KeyError
		var revoked *knownhosts.RevokedError
		switch {
		case err == nil:
		case errors.As(err, &revoked):
			keyFailure = result.Reason("host_key_revoked")
		case errors.As(err, &keyErr) && len(keyErr.Want) == 0:
			keyFailure = result.Reason("host_key_unknown")
		case errors.As(err, &keyErr):
			keyFailure = result.Reason("host_key_mismatch")
		default:
			keyFailure = result.Reason("host_key_unknown")
		}
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: time.Until(deadline)}).DialContext(ctx, "tcp", address)
	if err != nil {
		var netErr net.Error
		if ctx.Err() != nil || (errors.As(err, &netErr) && netErr.Timeout()) {
			return Outcome{SSH: result.Reason("timeout"), Exit: -1}
		}
		return Outcome{SSH: result.Reason("unreachable"), Exit: -1}
	}
	defer func() { _ = conn.Close() }()
	// Bound the handshake and the whole session by the host deadline.
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	clientConfig := &ssh.ClientConfig{User: s.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: callback,
		HostKeyAlgorithms: hostKeyAlgorithms(known, address), Timeout: time.Until(deadline)}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, clientConfig)
	if err != nil {
		switch {
		case keyFailure != "":
			return Outcome{SSH: keyFailure, Exit: -1}
		case ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded):
			return Outcome{SSH: result.Reason("timeout"), Exit: -1}
		case strings.Contains(err.Error(), "unable to authenticate"):
			return Outcome{SSH: result.Reason("auth_failed"), Exit: -1}
		}
		return Outcome{SSH: result.Reason("handshake_failed"), Exit: -1}
	}
	client := ssh.NewClient(sshConn, channels, requests)
	defer func() { _ = client.Close() }()
	outcome := Outcome{SSH: result.OK, SSHLatency: time.Since(start), Exit: -1}
	session, err := client.NewSession()
	if err != nil {
		return Outcome{SSH: result.Reason("session_failed"), Exit: -1}
	}
	defer func() { _ = session.Close() }()
	stdout := &limited{}
	session.Stdout, session.Stderr = stdout, io.Discard
	runStart := time.Now()
	// No command is sent: the authorized key forces the probe.
	if err := session.Shell(); err != nil {
		return Outcome{SSH: result.Reason("session_failed"), Exit: -1}
	}
	err = session.Wait()
	outcome.RunLatency = time.Since(runStart)
	var exitErr *ssh.ExitError
	switch {
	case err == nil:
		outcome.Exit = 0
	case errors.As(err, &exitErr) && exitErr.Signal() == "":
		outcome.Exit = exitErr.ExitStatus()
	case ctx.Err() != nil || time.Now().After(deadline):
		outcome.Timeout = true
	}
	stdout.mu.Lock()
	outcome.Output, outcome.Truncated = append([]byte(nil), stdout.buf.Bytes()...), stdout.truncated
	stdout.mu.Unlock()
	return outcome
}
