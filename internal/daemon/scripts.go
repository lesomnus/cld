package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/lesomnus/cld/internal/dockerx"
)

// scriptEvent is when a script runs.
type scriptEvent string

const (
	// scriptSetup runs once per container: after cld has installed claude and
	// restored state, before the session exists, so a tool it installs is
	// there from claude's first prompt. It re-runs when its own definition
	// changes, not on every reconcile.
	scriptSetup scriptEvent = "setup"
	// scriptStart runs once per container start generation, including the
	// first, for anything that has to happen again after a restart.
	scriptStart scriptEvent = "start"
)

// script is one script to run, with the config scope it came from for logs.
type script struct {
	spec   config.ScriptSpec
	origin string
}

// run_scripts runs the user's scripts for one event. It returns an error only
// for a script marked `on_error: fail`; anything else is logged and provisioning
// continues, because a personal script must not be able to lock the user out of
// a claude session.
//
// Note the timeout stops the daemon waiting; it does not kill the process in
// the container, which cld has no way to signal through a detached exec. The
// point is that a hanging script cannot stall the container's reconcile loop,
// which runs one thing at a time.
func (d *Daemon) run_scripts(ctx context.Context, e *entry, id string, ev scriptEvent) error {
	scripts := d.scripts_for(e, ev)
	if len(scripts) == 0 {
		return nil
	}

	sum := ""
	switch ev {
	case scriptStart:
		// Once per start generation. A container that restarts runs them again;
		// a reconcile that merely re-runs does not.
		if e.scripts_gen == e.started_at {
			return nil
		}
	case scriptSetup:
		// Once per container, keyed on what the scripts actually say. The marker
		// lives in the container, so it disappears with it and survives a daemon
		// restart — an in-memory flag would get both wrong.
		sum = scripts_hash(scripts)
		cur, ok, err := dockerx.ReadFile(ctx, d.cli, id, e.script_marker())
		if err == nil && ok && strings.TrimSpace(string(cur)) == sum {
			return nil
		}
	}

	for _, s := range scripts {
		if err := d.run_script(ctx, e, id, ev, s); err != nil {
			// Not marked done: `on_error: fail` says this script has to
			// succeed, so the next provisioning pass tries it again. Only a
			// fatal script gets here — a failure the user left as `warn` is
			// logged and counts as done.
			return err
		}
	}

	switch ev {
	case scriptStart:
		e.scripts_gen = e.started_at
	case scriptSetup:
		d.record_setup(ctx, e, id, sum)
	}
	return nil
}

// scripts_for collects the scripts that apply to this container for an event:
// the global one first, then every matching project block's, in file order.
// They accumulate rather than replace, so a global setup and a project's own
// both run.
func (d *Daemon) scripts_for(e *entry, ev scriptEvent) []script {
	out := []script{}
	add := func(set config.ScriptSet, origin string) {
		var spec *config.ScriptSpec
		switch ev {
		case scriptSetup:
			spec = set.Setup
		case scriptStart:
			spec = set.Start
		}
		if spec != nil {
			out = append(out, script{spec: *spec, origin: origin})
		}
	}

	add(d.cfg.Scripts, "cld.yaml scripts")
	for _, p := range d.cfg.MatchProjects(e.item.LocalFolder) {
		add(p.Scripts, "cld.yaml projects["+strings.Join(p.Match, ",")+"]")
	}
	return out
}

// run_script runs one script in the container, with the same environment the
// claude session gets plus the CLD_ variables describing what it is running
// for, as the container user (or whoever the script asked for) in the
// workspace.
func (d *Daemon) run_script(ctx context.Context, e *entry, id string, ev scriptEvent, s script) error {
	user := s.spec.User
	if user == "" {
		user = e.user
	}
	workdir := e.item.Workspace
	if s.spec.Workdir != "" {
		workdir = d.expand_container_path(e, s.spec.Workdir)
	}

	env := d.session_env(e)
	vars := append(env.Overrides(),
		"CLD_EVENT="+string(ev),
		"CLD_CONTAINER_ID="+id,
		"CLD_NAME="+e.item.Name,
		"CLD_WORKSPACE="+e.item.Workspace,
		"CLD_STARTED_AT="+e.started_at,
	)

	ctx, cancel := context.WithTimeout(ctx, s.spec.TimeoutOrDefault())
	defer cancel()

	log := d.log.With(
		slog.String("name", e.item.Name),
		slog.String("event", string(ev)),
		slog.String("origin", s.origin))
	log.Info("script running")

	out, code, err := dockerx.Exec(ctx, d.cli, id, dockerx.ExecOptions{
		User:       user,
		WorkingDir: workdir,
		Env:        vars,
		Cmd:        with_unset(env.Unset(), s.spec.Run.Cmd()),
	})
	if err == nil && code == 0 {
		log.Info("script done")
		return nil
	}

	reason := fmt.Sprintf("exit %d: %s", code, truncate(strings.TrimSpace(out), 2000))
	if err != nil {
		// A transport error, a cancelled daemon, or the timeout above.
		reason = err.Error()
	}
	if s.spec.Fatal() {
		return fmt.Errorf("%s %s: %s", s.origin, ev, reason)
	}
	log.Warn("script failed", slog.String("error", reason))
	return nil
}

// record_setup marks the setup scripts as done for this container. It runs
// even when one of them failed non-fatally: a script whose side effects landed
// halfway must not be re-applied on every reconcile, and `on_error: fail` is
// how a user says a failure matters enough to retry.
func (d *Daemon) record_setup(ctx context.Context, e *entry, id, sum string) {
	dir := path.Dir(e.script_marker())
	if err := d.mkdir_in_container(ctx, e, id, dir); err != nil {
		d.log.Warn("scripts: cannot record setup",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
		return
	}
	err := dockerx.WriteFile(ctx, d.cli, id, dir, path.Base(e.script_marker()),
		0o600, e.uid, e.gid, []byte(sum))
	if err != nil {
		d.log.Warn("scripts: cannot record setup",
			slog.String("name", e.item.Name), slog.String("error", err.Error()))
	}
}

// script_marker records the setup scripts a container was last provisioned
// with, next to the file placement markers.
func (e *entry) script_marker() string {
	return path.Join(e.cache_home, "cld", "scripts", "setup.sha256")
}

// scripts_hash digests the whole ordered list, so adding, removing, editing or
// reordering any of them re-runs the lot. Per-script markers would be finer,
// but a user who edits one setup script expects it to take effect, and running
// all of them again is the behaviour that never silently skips.
func scripts_hash(scripts []script) string {
	h := sha256.New()
	for _, s := range scripts {
		fmt.Fprintf(h, "origin=%s\nuser=%s\nworkdir=%s\ncmd=%q\n",
			s.origin, s.spec.User, s.spec.Workdir, s.spec.Run.Cmd())
	}
	return hex.EncodeToString(h.Sum(nil))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "... (truncated)"
}
