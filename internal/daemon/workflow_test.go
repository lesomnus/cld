package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseWorkflowRuns(t *testing.T) {
	// Fields: run_id  started  result  updated_mtime  finalized  status  name
	out := strings.Join([]string{
		// in-flight run: no state file, so finalized=0 and status/name blank.
		"wf_live\t5\t2\t1784291500\t0\t\t",
		// completed run finalized with its state file.
		"wf_done\t4\t4\t1784291520\t1\tcompleted\tverify-workflow-datamodel",
		// aborted run: finalized but an agent never resulted.
		"wf_abort\t4\t3\t1784200000\t1\tfailed\tflaky",
		"\t1\t1\t1\t1\tx\ty",       // empty run id -> skipped
		"not a real line",          // fewer than 4 fields -> skipped
		"wf_bad\tNaN\t0\t0\t0\t\t", // non-numeric count -> skipped
	}, "\n")

	runs := parseWorkflowRuns(out)
	require.Len(t, runs, 3)

	// Ordered newest-first by journal mtime.
	require.Equal(t, []string{"wf_done", "wf_live", "wf_abort"},
		[]string{runs[0].RunID, runs[1].RunID, runs[2].RunID})

	done := runs[0]
	require.Equal(t, 4, done.Total)
	require.Equal(t, 4, done.Done)
	require.Equal(t, 0, done.Running())
	require.True(t, done.Finalized)
	require.Equal(t, "completed", done.Status)
	require.Equal(t, "verify-workflow-datamodel", done.Name)
	require.False(t, done.UpdatedAt.IsZero())

	live := runs[1]
	require.False(t, live.Finalized)
	require.Equal(t, "", live.Status)
	require.Equal(t, 3, live.Running())

	abort := runs[2]
	require.True(t, abort.Finalized)
	require.Equal(t, "failed", abort.Status)
	require.Equal(t, 1, abort.Running(), "one agent never resulted")
}

func TestWorkflowRunsEqual(t *testing.T) {
	a := []WorkflowRun{{RunID: "wf_a", Total: 2, Done: 1}}
	require.True(t, workflowRunsEqual(a, a))
	require.False(t, workflowRunsEqual(a, nil))
	require.False(t, workflowRunsEqual(a, []WorkflowRun{{RunID: "wf_a", Total: 2, Done: 2}}))
}
