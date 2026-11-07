package state

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zekihan/argus/internal/kube"
)

const empty = `{"version":2,"entries":{}}`

func TestConditionalSaveAndReload(t *testing.T) {
	backend := NewMemory(empty, true)
	store := New(backend)
	if err := store.Save(t.Context(), Ledger{}); !errors.Is(err, ErrNotLoaded) {
		t.Fatal("write without a read accepted", err)
	}
	if ledger, err := store.Load(t.Context()); err != nil || len(ledger) != 0 {
		t.Fatal(ledger, err)
	}
	now := time.Now().UTC()
	want := Ledger{"db/db/repo1": {UID: "uid", RepositoryID: "destination", Created: now.Add(-time.Hour), LastSuccess: now, Source: "Completed Job"}}
	if err := store.Save(t.Context(), want); err != nil {
		t.Fatal(err)
	}
	// Consecutive saves chain on the version each write returned.
	if err := store.Save(t.Context(), want); err != nil || backend.Writes() != 2 {
		t.Fatal(err, backend.Writes())
	}
	got, err := New(backend).Load(t.Context())
	if err != nil || !got["db/db/repo1"].LastSuccess.Equal(now) || !Equal(got, want) {
		t.Fatal(got, err)
	}
	// Another writer changes the document: the next conditional write conflicts.
	backend.Set(empty)
	if err := store.Save(t.Context(), want); !kube.IsConflict(err) {
		t.Fatal("stale write accepted", err)
	}
	if err := store.Save(t.Context(), want); !errors.Is(err, ErrNotLoaded) {
		t.Fatal("write after a conflict must re-read first", err)
	}
}

func TestTimeoutAfterCommitRequiresReread(t *testing.T) {
	backend := NewMemory(empty, true)
	backend.AfterWrite = func(int) error { return &kube.Error{Code: kube.CodeTimeout, Path: "x"} }
	store := New(backend)
	if _, err := store.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	ledger := Ledger{"db/db/repo1": {UID: "uid", RepositoryID: "destination", Created: time.Unix(0, 0).UTC(), NotBefore: time.Unix(10, 0).UTC()}}
	if err := store.Save(t.Context(), ledger); kube.CodeOf(err) != kube.CodeTimeout {
		t.Fatal(err)
	}
	reread, err := store.Load(t.Context())
	if err != nil || !Equal(reread, ledger) {
		t.Fatal("committed write not visible on read-back", reread, err)
	}
}

func TestCorruptMissingAndOversizedStateFailClosed(t *testing.T) {
	for _, value := range []string{``, `{`, `{"version":3,"entries":{}}`, `{"version":1,"entries":null}`, `{"version":1,"version":1,"entries":{}}`,
		`{"version":1,"entries":{}} {}`, `{"version":1,"entries":{"x":{}}}`, `{"version":2,"entries":{},"extra":1}`,
		`{"version":2,"entries":{}}` + strings.Repeat(" ", MaxBytes)} {
		backend := NewMemory(value, true)
		store := New(backend)
		if _, err := store.Load(t.Context()); !errors.Is(err, ErrInvalid) {
			t.Fatal("corrupt state accepted", value[:min(len(value), 40)], err)
		}
		if err := store.Save(t.Context(), Ledger{}); err == nil || backend.Data() != value {
			t.Fatal("corrupt state overwritten")
		}
	}
	if _, err := New(NewMemory("", false)).Load(t.Context()); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing key accepted", err)
	}
	if _, err := New(NewMemory(empty, true).Missing()).Load(t.Context()); !errors.Is(err, ErrMissing) {
		t.Fatal("missing ConfigMap accepted", err)
	}
	big := Ledger{}
	for i := range 1500 {
		big[fmt.Sprintf("namespace-%04d/cluster-%04d/repo1", i, i)] = Entry{UID: strings.Repeat("u", 36), RepositoryID: strings.Repeat("a", 64), Created: time.Unix(0, 0).UTC()}
	}
	if _, err := Encode(big); !errors.Is(err, ErrTooLarge) {
		t.Fatal("oversized ledger encoded", err)
	}
	backend := NewMemory(empty, true)
	store := New(backend)
	if _, err := store.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), big); !errors.Is(err, ErrTooLarge) || backend.Data() != empty {
		t.Fatal("oversized ledger written", err)
	}
}

func TestLegacyAndIdentityOnlyCheckpoints(t *testing.T) {
	legacy := `{"version":1,"entries":{"db/db/repo1":{"uid":"uid","created":"2026-01-01T00:00:00Z","lastSuccess":"2026-10-02T11:00:00Z","source":"Completed Job"}}}`
	old, err := Decode([]byte(legacy))
	if err != nil || old["db/db/repo1"].RepositoryID != "" {
		t.Fatal(old, err)
	}
	birth := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := birth.Add(time.Hour)
	entry := Entry{UID: "uid", Created: birth, RepositoryID: "destination", NotBefore: cutoff}
	data, err := Encode(Ledger{"db/db/repo1": entry})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil || !got["db/db/repo1"].NotBefore.Equal(cutoff) || !got["db/db/repo1"].LastSuccess.IsZero() {
		t.Fatal(got, err)
	}
	for _, mutate := range []func(*Entry){
		func(e *Entry) { e.RepositoryID = "" },
		func(e *Entry) { e.Source = "impossible success" },
		func(e *Entry) { e.NotBefore = birth.Add(-time.Second) },
		func(e *Entry) { e.LastSuccess = cutoff; e.Source = "Completed Job" },
		func(e *Entry) { e.LastSuccess = cutoff.Add(time.Second) },
	} {
		bad := entry
		mutate(&bad)
		data, err := Encode(Ledger{"db/db/repo1": bad})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(data); err == nil {
			t.Fatal("invalid identity checkpoint accepted", bad)
		}
	}
}

func TestExportedBackrestwatchLedgersValidate(t *testing.T) {
	for file, entries := range map[string]int{"testdata/backrestwatch-v1.json": 1, "testdata/backrestwatch-v2.json": 20} {
		ledger, err := ValidateFile(file)
		if err != nil || len(ledger) != entries {
			t.Fatal(file, len(ledger), err)
		}
	}
}
