package daemon

import (
	"testing"
	"time"

	"github.com/lesomnus/cld/cmd/config"
	"github.com/stretchr/testify/require"
)

func spec(run string) *config.ScriptSpec {
	return &config.ScriptSpec{Run: config.ScriptRun{Shell: run}}
}

func TestScriptsFor(t *testing.T) {
	d, cfg := newTestDaemon(t)
	cfg.Scripts = config.ScriptSet{Setup: spec("global-setup"), Start: spec("global-start")}
	cfg.Projects = []config.ProjectConfig{
		{Match: config.StringList{"/work/**"}, Scripts: config.ScriptSet{Setup: spec("project-setup")}},
		{Match: config.StringList{"/nope/**"}, Scripts: config.ScriptSet{Setup: spec("other-setup")}},
	}
	e := &entry{item: Item{LocalFolder: "/work/api"}}

	t.Run("global and project scripts accumulate, global first", func(t *testing.T) {
		got := d.scripts_for(e, scriptSetup)
		require.Len(t, got, 2)
		require.Equal(t, "global-setup", got[0].spec.Run.Shell)
		require.Equal(t, "cld.yaml scripts", got[0].origin)
		require.Equal(t, "project-setup", got[1].spec.Run.Shell)
		require.Equal(t, "cld.yaml projects[/work/**]", got[1].origin)
	})

	t.Run("events are separate", func(t *testing.T) {
		got := d.scripts_for(e, scriptStart)
		require.Len(t, got, 1)
		require.Equal(t, "global-start", got[0].spec.Run.Shell)
	})

	t.Run("a non-matching project contributes nothing", func(t *testing.T) {
		got := d.scripts_for(&entry{item: Item{LocalFolder: "/elsewhere"}}, scriptSetup)
		require.Len(t, got, 1)
	})
}

func TestScriptsHash(t *testing.T) {
	base := []script{{spec: *spec("a"), origin: "o"}}

	t.Run("is stable", func(t *testing.T) {
		require.Equal(t, scripts_hash(base), scripts_hash([]script{{spec: *spec("a"), origin: "o"}}))
	})

	t.Run("changes when the command changes", func(t *testing.T) {
		require.NotEqual(t, scripts_hash(base), scripts_hash([]script{{spec: *spec("b"), origin: "o"}}))
	})

	t.Run("changes when a script is added", func(t *testing.T) {
		two := append(append([]script{}, base...), script{spec: *spec("b"), origin: "o"})
		require.NotEqual(t, scripts_hash(base), scripts_hash(two))
	})

	t.Run("changes when the user or workdir changes", func(t *testing.T) {
		asRoot := []script{{spec: config.ScriptSpec{Run: config.ScriptRun{Shell: "a"}, User: "root"}, origin: "o"}}
		require.NotEqual(t, scripts_hash(base), scripts_hash(asRoot))
	})

	t.Run("ignores fields that do not change what runs", func(t *testing.T) {
		// Re-running everything because a timeout was tuned would be surprising.
		tuned := []script{{
			spec:   config.ScriptSpec{Run: config.ScriptRun{Shell: "a"}, Timeout: config.Duration(time.Minute)},
			origin: "o",
		}}
		require.Equal(t, scripts_hash(base), scripts_hash(tuned))
	})

	t.Run("distinguishes a shell line from an argv list", func(t *testing.T) {
		argv := []script{{spec: config.ScriptSpec{Run: config.ScriptRun{Argv: []string{"a"}}}, origin: "o"}}
		require.NotEqual(t, scripts_hash(base), scripts_hash(argv))
	})
}

// With nothing configured, the whole feature must cost nothing: no marker
// read, no exec — which is what an early return with no docker client proves.
func TestScriptsNoneConfigured(t *testing.T) {
	d, _ := newTestDaemon(t)
	require.Nil(t, d.cli, "the test daemon has no docker client")
	require.NoError(t, d.run_scripts(t.Context(), &entry{}, "ctr", scriptSetup))
	require.NoError(t, d.run_scripts(t.Context(), &entry{}, "ctr", scriptStart))
}

func TestTruncate(t *testing.T) {
	require.Equal(t, "abc", truncate("abc", 3))
	require.Equal(t, "ab... (truncated)", truncate("abc", 2))
}
