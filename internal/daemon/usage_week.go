package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// weeklyWindow is the fallback window length used when no subscription weekly
// reset time is known (offline, or before the first usage report): the totals
// then roll over 7 days after they started, mirroring the weekly limit's span.
const weeklyWindow = 7 * 24 * time.Hour

// WeeklyUsage is the fleet-wide token consumption accumulated over the current
// weekly window — the "this week" figure shown at the bottom of `cld watch`.
//
// Unlike the per-container telemetry (in-memory, forgotten when a container goes
// away), this survives a daemon restart and is zeroed only when the weekly
// window resets, so it tracks a full week the way the subscription's weekly
// limit does. Only the three token types the bottom line shows are kept; cost
// and cacheRead are deliberately left out.
type WeeklyUsage struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheCreation int64 `json:"cache_creation"`
	// ResetsAt is the weekly-window boundary this total counts toward, adopted
	// from the subscription's SevenDay usage window. Zero until a usage report
	// has supplied one, in which case the window falls back to weeklyWindow after
	// Since.
	ResetsAt time.Time `json:"resets_at,omitzero"`
	// Since is when the current window started accumulating.
	Since time.Time `json:"since,omitzero"`
}

// Empty reports whether nothing has been accumulated yet, so the bottom line can
// collapse rather than print three zeros for a window that never saw a token.
func (w WeeklyUsage) Empty() bool {
	return w.Input == 0 && w.Output == 0 && w.CacheCreation == 0
}

// weeklyStore accumulates WeeklyUsage and persists it to a JSON file, rolling
// over to a fresh window when the weekly boundary passes. Every method tolerates
// a nil receiver (reading as "nothing collected"), so a Daemon assembled
// field-by-field in a test does not panic merely for listing usage.
type weeklyStore struct {
	mu   sync.Mutex
	path string
	now  func() time.Time // overridable in tests
	cur  WeeklyUsage
}

// newWeeklyStore loads any persisted totals from path (a missing or unreadable
// file simply starts empty) and returns a store that writes back to it.
func newWeeklyStore(path string) *weeklyStore {
	s := &weeklyStore{path: path, now: time.Now}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &s.cur) // a corrupt file starts from zero
	}
	return s
}

// boundary is when the current window rolls over: the adopted weekly reset when
// known, else weeklyWindow after it started, else unknown (nothing accumulated).
func (s *weeklyStore) boundary() time.Time {
	if !s.cur.ResetsAt.IsZero() {
		return s.cur.ResetsAt
	}
	if !s.cur.Since.IsZero() {
		return s.cur.Since.Add(weeklyWindow)
	}
	return time.Time{}
}

// rollover zeroes the window if now has reached its boundary, returning whether
// it did. The adopted reset is cleared so the next anchor supplies the new one;
// until then the fallback window applies. The caller holds the lock.
func (s *weeklyStore) rollover(now time.Time) bool {
	b := s.boundary()
	if b.IsZero() || now.Before(b) {
		return false
	}
	s.cur = WeeklyUsage{Since: now}
	return true
}

// add folds one export's token deltas into the current window, rolling it over
// first if the boundary has passed. Zero deltas still stamp the window's start
// so an anchor-less session eventually rolls over on the fallback schedule.
func (s *weeklyStore) add(in, out, cw int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	s.rollover(now)
	if s.cur.Since.IsZero() {
		s.cur.Since = now
	}
	s.cur.Input += in
	s.cur.Output += out
	s.cur.CacheCreation += cw
	s.save()
}

// anchor adopts the subscription's weekly reset time as the window boundary,
// rolling over first if the previously adopted one has already passed. A zero
// resetsAt (endpoint reported none) leaves the fallback window in place.
func (s *weeklyStore) anchor(resetsAt time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	changed := s.rollover(now)
	if !resetsAt.IsZero() && !resetsAt.Equal(s.cur.ResetsAt) {
		s.cur.ResetsAt = resetsAt
		if s.cur.Since.IsZero() {
			s.cur.Since = now
		}
		changed = true
	}
	if changed {
		s.save()
	}
}

// get returns the current window, rolling it over on read so a window whose
// boundary passed while the daemon was idle reads as zero even before the next
// export or anchor arrives.
func (s *weeklyStore) get() WeeklyUsage {
	if s == nil {
		return WeeklyUsage{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rollover(s.now()) {
		s.save()
	}
	return s.cur
}

// save writes the current window to disk atomically (temp file + rename) so a
// crash mid-write never leaves a torn file. Best-effort: a write that fails
// leaves the in-memory total intact and is simply retried on the next update.
func (s *weeklyStore) save() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.cur)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// weeklyReset picks the subscription weekly reset the aggregate window should
// anchor to: the first source with a SevenDay reset, which is broker-first (see
// usageTargets), so the shared broker login's week wins when present.
func weeklyReset(report *UsageReport) time.Time {
	if report == nil {
		return time.Time{}
	}
	for _, s := range report.Sources {
		if s.Usage != nil && !s.Usage.SevenDay.ResetsAt.IsZero() {
			return s.Usage.SevenDay.ResetsAt
		}
	}
	return time.Time{}
}
