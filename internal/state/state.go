// Package state stores the pgBackRest completion ledger in one ConfigMap key.
//
// Every write is conditional on the resourceVersion of the last read. A
// corrupt or unknown-version document is reported as ErrInvalid and is never
// overwritten. Convergence after a conflict (re-read, adopt, re-evaluate) is
// the caller's job, because only the caller can re-evaluate its sample.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"time"

	"github.com/zekihan/argus/internal/config"
	"github.com/zekihan/argus/internal/kube"
)

// Key is the ConfigMap data key holding the ledger.
const Key = "pgbackrest.json"

// MaxBytes keeps the document under the 1 MiB ConfigMap limit and the 256 KiB
// client-side-apply annotation limit.
const MaxBytes = 200 * 1024

// MaxEntries matches backrestwatch's ledger bound.
const MaxEntries = 4000

var (
	// ErrInvalid marks a corrupt, oversized or unknown-version document.
	ErrInvalid = errors.New("state document is corrupt, oversized or of an unknown version")
	// ErrMissing marks an absent ConfigMap.
	ErrMissing = errors.New("state ConfigMap is missing")
	// ErrTooLarge marks a ledger that would exceed MaxBytes when written.
	ErrTooLarge = errors.New("state ledger exceeds the size limit")
	// ErrNotLoaded marks a write without a prior successful read.
	ErrNotLoaded = errors.New("state was not read before writing")
)

type Entry struct {
	RepositoryID string    `json:"repositoryID,omitempty"`
	NotBefore    time.Time `json:"notBefore,omitzero"`
	UID          string    `json:"uid"`
	Created      time.Time `json:"created"`
	LastSuccess  time.Time `json:"lastSuccess"`
	Source       string    `json:"source"`
}

type Ledger map[string]Entry

type document struct {
	Version int    `json:"version"`
	Entries Ledger `json:"entries"`
}

// Decode validates a ledger document with backrestwatch's rules for both
// ledger versions 1 and 2, and Argus's size cap.
func Decode(data []byte) (Ledger, error) {
	if len(data) > MaxBytes || config.UniqueJSON(data) != nil {
		return nil, ErrInvalid
	}
	var doc document
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&doc) != nil || (doc.Version != 1 && doc.Version != 2) || doc.Entries == nil ||
		len(doc.Entries) > MaxEntries {
		return nil, ErrInvalid
	}
	for key, entry := range doc.Entries {
		if !validEntry(doc.Version, key, entry) {
			return nil, ErrInvalid
		}
	}
	return doc.Entries, nil
}

// validEntry checks one ledger entry against its document version's rules.
func validEntry(version int, key string, entry Entry) bool {
	if version == 1 {
		return entry.RepositoryID == "" && entry.NotBefore.IsZero() && !entry.LastSuccess.IsZero()
	}
	if !validFields(key, entry) || !validTimeline(entry) {
		return false
	}
	return true
}

// validFields checks the version-2 identity fields of one entry.
func validFields(key string, entry Entry) bool {
	if entry.RepositoryID == "" || key == "" || entry.UID == "" || entry.Created.IsZero() {
		return false
	}
	if !entry.LastSuccess.IsZero() {
		if entry.LastSuccess.Before(entry.Created) || entry.Source == "" {
			return false
		}
	} else if entry.RepositoryID == "" || entry.Source != "" {
		return false
	}
	return true
}

// validTimeline checks the notBefore barrier against creation and success.
func validTimeline(entry Entry) bool {
	if entry.NotBefore.IsZero() {
		return true
	}
	if entry.NotBefore.Before(entry.Created) {
		return false
	}
	if !entry.LastSuccess.IsZero() && !entry.LastSuccess.After(entry.NotBefore) {
		return false
	}
	return true
}

// Encode writes a version 2 document, refusing one that could not be loaded back.
func Encode(entries Ledger) ([]byte, error) {
	if entries == nil {
		entries = Ledger{}
	}
	if len(entries) > MaxEntries {
		return nil, ErrTooLarge
	}
	data, err := json.Marshal(document{Version: 2, Entries: entries})
	if err != nil {
		return nil, errors.New("encode state ledger")
	}
	if len(data) > MaxBytes {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Equal reports whether two ledgers encode identically.
func Equal(a, b Ledger) bool {
	left, errLeft := json.Marshal(document{Version: 2, Entries: a})
	right, errRight := json.Marshal(document{Version: 2, Entries: b})
	return errLeft == nil && errRight == nil && bytes.Equal(left, right)
}

// ValidateFile checks an exported ledger (backrestwatch evidence.json) for import.
func ValidateFile(path string) (Ledger, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, config.FileError("open ledger file", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return nil, errors.New("read ledger file")
	}
	return Decode(data)
}

// Backend is the conditional storage under a Store.
type Backend interface {
	// Read returns the stored document, whether the key exists, and a version token.
	Read(ctx context.Context) (data string, present bool, version string, err error)
	// Write stores data only if the stored version still equals version.
	Write(ctx context.Context, data, version string) (newVersion string, err error)
}

// Store loads and conditionally saves the ledger through a Backend.
type Store struct {
	backend Backend
	version string
}

func New(backend Backend) *Store { return &Store{backend: backend} }

// Load reads the ledger and remembers its version for the next Save.
// A missing key is invalid: the chart always creates the document.
func (s *Store) Load(ctx context.Context) (Ledger, error) {
	data, present, version, err := s.backend.Read(ctx)
	if err != nil {
		s.version = ""
		return nil, err
	}
	s.version = ""
	if !present {
		return nil, ErrInvalid
	}
	ledger, err := Decode([]byte(data))
	if err != nil {
		return nil, err
	}
	s.version = version
	return ledger, nil
}

// Save writes the ledger conditionally on the version of the last Load or Save.
func (s *Store) Save(ctx context.Context, ledger Ledger) error {
	if s.version == "" {
		return ErrNotLoaded
	}
	data, err := Encode(ledger)
	if err != nil {
		return err
	}
	version, err := s.backend.Write(ctx, string(data), s.version)
	if err != nil {
		// The write may or may not have landed; only a re-read can tell.
		s.version = ""
		return err
	}
	s.version = version
	return nil
}

// ConfigMap stores the ledger in one key of a ConfigMap.
type ConfigMap struct {
	Client    *kube.Client
	Namespace string
	Name      string
	last      kube.ConfigMap
}

func (c *ConfigMap) Read(ctx context.Context) (string, bool, string, error) {
	current, err := c.Client.GetConfigMap(ctx, c.Namespace, c.Name)
	if kube.IsNotFound(err) {
		return "", false, "", ErrMissing
	}
	if err != nil {
		return "", false, "", err
	}
	c.last = current
	data, present := current.Data[Key]
	return data, present, current.ResourceVersion, nil
}

func (c *ConfigMap) Write(ctx context.Context, data, version string) (string, error) {
	if c.last.ResourceVersion != version {
		return "", ErrNotLoaded
	}
	next := make(map[string]string, len(c.last.Data)+1)
	maps.Copy(next, c.last.Data)
	next[Key] = data
	updated, err := c.Client.UpdateConfigMap(ctx, c.Namespace, c.Name, c.last, next)
	// A successful PUT can return a ledger changed by admission.
	if stored, present := updated.Data[Key]; err == nil && (!present || stored != data) {
		err = &kube.Error{Code: kube.CodeInvalid}
	}
	if err != nil {
		c.last = kube.ConfigMap{}
		return "", err
	}
	c.last = updated
	return updated.ResourceVersion, nil
}
