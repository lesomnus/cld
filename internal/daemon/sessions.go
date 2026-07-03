package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// sessionState records, per container, which generation a session was
// established for and whether the user ended it. It is persisted so a daemon
// restart does not resurrect a session the user closed: the generation key is
// the container's StartedAt, which survives a daemon restart and changes when
// the container itself restarts.
type sessionState struct {
	Gen   string `json:"gen"`   // container StartedAt the session corresponds to
	Ended bool   `json:"ended"` // the user ended claude in that generation
}

type sessionStore struct {
	dir string
}

func (s *sessionStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func (s *sessionStore) get(id string) sessionState {
	var st sessionState
	data, err := os.ReadFile(s.path(id))
	if err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

func (s *sessionStore) set(id string, st sessionState) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return os.WriteFile(s.path(id), data, 0o600)
}

func (s *sessionStore) clear(id string) {
	os.Remove(s.path(id))
}
