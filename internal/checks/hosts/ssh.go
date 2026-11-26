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

// outcomeReason maps a probe failure phase to its reason code.
func outcomeReason(reason string) Outcome {
	return Outcome{SSH: result.Reason(reason), Exit: -1}
}

// hostDialer dials the host and reports a timeout outcome for deadline errors.
func hostDialer(ctx context.Context, address string, deadline time.Time) (net.Conn, string) {
	conn, err := (&net.Dialer{Timeout: time.Until(deadline)}).DialContext(ctx, "tcp", address)
	if err != nil {
		var netErr net.Error
		if ctx.Err() != nil || (errors.As(err, &netErr) && netErr.Timeout()) {
			return nil, "timeout"
		}
		return nil, "unreachable"
	}
	return conn, ""
}

// handshakeOutcome maps a handshake failure to its probe outcome.
func handshakeOutcome(ctx context.Context, err error, keyFailure string) Outcome {
	switch {
	case keyFailure != "":
		return Outcome{SSH: keyFailure, Exit: -1}
	case ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded):
		return outcomeReason("timeout")
	case strings.Contains(err.Error(), "unable to authenticate"):
		return outcomeReason("auth_failed")
	}
	return outcomeReason("handshake_failed")
}

func (s SSH) Probe(ctx context.Context, host config.Host) Outcome {
	keyData, err := readSmall(s.PrivateKeyFile, 64*1024)
	if err != nil {
		return outcomeReason("key_unavailable")
	}
	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return outcomeReason("key_unavailable")
	}
	known, err := knownhosts.New(s.KnownHostsFile)
	if err != nil {
		return outcomeReason("known_hosts_unavailable")
	}
	address, err := config.HostAddress(host.Address)
	if err != nil {
		return outcomeReason("unreachable")
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
	conn, reason := hostDialer(ctx, address, deadline)
	if reason != "" {
		return outcomeReason(reason)
	}
	defer func() { _ = conn.Close() }()
	// Bound the handshake and the whole session by the host deadline.
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return Outcome{SSH: result.Reason("unreachable"), Exit: -1}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	clientConfig := &ssh.ClientConfig{
		User: s.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: callback,
		HostKeyAlgorithms: hostKeyAlgorithms(known, address), Timeout: time.Until(deadline),
	}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, clientConfig)
	if err != nil {
		return handshakeOutcome(ctx, err, keyFailure)
	}
	client := ssh.NewClient(sshConn, channels, requests)
	defer func() { _ = client.Close() }()
	outcome := Outcome{SSH: result.OK, SSHLatency: time.Since(start), Exit: -1}
	outcome.RunLatency = sessionOutcome(ctx, client, deadline, &outcome)
	return outcome
}

// sessionOutcome runs the shell and records the outcome's exit and output.
func sessionOutcome(
	ctx context.Context,
	client *ssh.Client,
	deadline time.Time,
	outcome *Outcome,
) time.Duration {
	stdout := &limited{}
	runStart := time.Now()
	session, err := client.NewSession()
	if err == nil {
		defer func() { _ = session.Close() }()
		session.Stdout, session.Stderr = stdout, io.Discard
		// No command is sent: the authorized key forces the probe.
		err = session.Shell()
	}
	if err != nil {
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			outcome.Timeout = true
			outcome.Output, outcome.Truncated = append(
				[]byte(nil),
				stdout.buf.Bytes()...), stdout.truncated
			return time.Since(runStart)
		}
		outcome.SSH = result.Reason("session_failed")
		return 0
	}
	err = session.Wait()
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
	return time.Since(runStart)
}
