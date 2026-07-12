package broker

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// FileStore persists the broker's rotating credentials to a single JSON file,
// mode 0600, written atomically (temp + rename) so a concurrent read never sees
// a half-written token. A missing file loads as empty credentials (no refresh
// token yet), not an error, so a fresh install is a normal state.
type FileStore struct{ Path string }

func (s FileStore) Load() (*Credentials, error) {
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return &Credentials{}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s FileStore) Save(c *Credentials) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
