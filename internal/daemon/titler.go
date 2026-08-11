package daemon

import (
	"context"
	"time"

	"github.com/lesomnus/cld/internal/devc"
)

// The outer terminal's tab title is driven from each session's tmux window name
// (set-titles-string "#W", see tmuxx.tune). A plain shell tab and a cld/claude
// tab would otherwise read alike, so every managed window carries a leading
// glyph: titleGlyph marks the tab as claude's, and while the conversation is
// generating the glyph pulses through spinnerFrames — a dot swelling into the
// star and back — echoing Claude Code's own working spinner. When the session
// is waiting or idle the glyph rests on the static titleGlyph.
const titleGlyph = "✻"

// spinnerFrames pulse a dot up into the ✻ star and back down. The sequence is a
// palindrome so it breathes evenly, and every glyph is a text-presentation one
// from the Dingbats block the container renders under LC_ALL=C.UTF-8 — no
// emoji-default codepoints (e.g. ✳ U+2733), which a terminal would widen and
// color instead of drawing as a plain glyph.
var spinnerFrames = []string{"·", "✢", "✷", "✻", "✽", "✻", "✷", "✢"}

// titleFrameInterval is how often a working session advances one spinner frame.
// 500ms reads as a calm pulse without renaming the window more than needed.
const titleFrameInterval = 500 * time.Millisecond

// windowName decorates a session's display label with its leading tab glyph.
// frame is titleGlyph when at rest or a spinnerFrames entry while working.
func windowName(frame, display string) string {
	if display == "" {
		return frame
	}
	return frame + " " + display
}

// titleState is the animator's per-session memory: which spinner frame the
// session is on and the last name actually pushed to tmux, so a resting glyph
// is renamed once rather than every tick.
type titleState struct {
	frame int
	last  string
}

// run_titler animates every ready session's tab glyph until ctx is cancelled.
// It is the sole writer of the window name after ensure sets the initial one,
// so it never contends with the worker: it reads only the published snapshot
// (e.snap) and drives tmux directly. Best-effort throughout — a rename that
// fails is retried next tick, and a cosmetic tab is never worth a hard error.
func (d *Daemon) run_titler(ctx context.Context) {
	t := time.NewTicker(titleFrameInterval)
	defer t.Stop()

	states := map[string]*titleState{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.tick_titles(ctx, states)
		}
	}
}

// tick_titles advances one frame for each ready session. A working session
// steps to its next spinner glyph; any other rests on titleGlyph. states is
// keyed by container id and pruned as sessions come and go.
//
// Activity is read from the published snapshot, so only push-capable sessions
// (claude's in-container hooks report their state) ever animate — a poll-only
// session has no live Activity in the snapshot and simply wears the static
// glyph, which still tells its tab apart from a plain terminal.
func (d *Daemon) tick_titles(ctx context.Context, states map[string]*titleState) {
	type target struct {
		id      string
		session string
		display string
		working bool
	}

	d.mu.Lock()
	targets := make([]target, 0, len(d.entries))
	for id, e := range d.entries {
		it := e.snap.Load()
		if it == nil || it.Status != StatusReady {
			continue
		}
		display := it.Display
		if display == "" {
			display = it.Name
		}
		targets = append(targets, target{
			id:      id,
			session: devc.SessionName(it.Name),
			display: display,
			working: it.Activity == ActivityWorking,
		})
	}
	d.mu.Unlock()

	seen := make(map[string]bool, len(targets))
	for _, tg := range targets {
		seen[tg.id] = true
		st := states[tg.id]
		if st == nil {
			st = &titleState{}
			states[tg.id] = st
		}

		frame := titleGlyph
		if tg.working {
			st.frame = (st.frame + 1) % len(spinnerFrames)
			frame = spinnerFrames[st.frame]
		} else {
			st.frame = 0
		}

		name := windowName(frame, tg.display)
		if name == st.last {
			continue
		}
		if err := d.tmux.RenameWindow(ctx, tg.session, name); err != nil {
			// Clear the cache so the next tick retries rather than assuming the
			// tab now shows a name tmux rejected.
			st.last = ""
			continue
		}
		st.last = name
	}

	for id := range states {
		if !seen[id] {
			delete(states, id)
		}
	}
}
