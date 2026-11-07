package pgbackrest

import (
	"errors"
	"io/fs"
	"os"

	"github.com/zekihan/argus/internal/state"
)

// fileStore round-trips a ledger through the exact ConfigMap document
// encoding, standing in for backrestwatch's file checkpoint in ported tests.
type fileStore struct{ path string }

func openStore(path string) (*fileStore, error) { return &fileStore{path: path}, nil }

func (f *fileStore) Save(ledger state.Ledger) error {
	data, err := state.Encode(ledger)
	if err != nil {
		return err
	}
	return os.WriteFile(f.path, data, 0600)
}

func (f *fileStore) Load() (state.Ledger, error) {
	data, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return state.Ledger{}, nil
	}
	if err != nil {
		return nil, err
	}
	return state.Decode(data)
}

func (f *fileStore) Close() error { return nil }
