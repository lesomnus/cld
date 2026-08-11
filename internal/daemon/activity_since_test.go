package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPublishStampsTransitions verifies publish() marks StatusSince /
// ActivitySince only when the corresponding field actually changes — an
// unrelated republish (e.g. a title refresh) must leave the marks put, so the
// listing's FOR duration keeps counting from the real transition.
func TestPublishStampsTransitions(t *testing.T) {
	d := &Daemon{entries: map[string]*entry{}}
	e := d.get_or_create("cid")

	e.item.Status = StatusProvisioning
	e.publish()
	s0 := e.snapshot()
	require.False(t, s0.StatusSince.IsZero(), "first publish stamps StatusSince")
	require.False(t, s0.ActivitySince.IsZero(), "first publish stamps ActivitySince")

	// A republish that changes only the title must not move either mark.
	time.Sleep(time.Millisecond)
	e.item.Title = "some title"
	e.publish()
	s1 := e.snapshot()
	require.True(t, s1.StatusSince.Equal(s0.StatusSince), "title change leaves StatusSince")
	require.True(t, s1.ActivitySince.Equal(s0.ActivitySince), "title change leaves ActivitySince")

	// Going ready + working moves both marks (status and activity both change).
	time.Sleep(time.Millisecond)
	e.item.Status = StatusReady
	e.item.Activity = ActivityWorking
	e.publish()
	s2 := e.snapshot()
	require.True(t, s2.StatusSince.After(s1.StatusSince), "status change moves StatusSince")
	require.True(t, s2.ActivitySince.After(s1.ActivitySince), "activity change moves ActivitySince")

	// Activity working -> waiting moves ActivitySince only; Status held ready.
	time.Sleep(time.Millisecond)
	e.item.Activity = ActivityWaiting
	e.publish()
	s3 := e.snapshot()
	require.True(t, s3.StatusSince.Equal(s2.StatusSince), "steady status leaves StatusSince")
	require.True(t, s3.ActivitySince.After(s2.ActivitySince), "activity change moves ActivitySince")
}

// TestPublishAccumulatesWorkingTime verifies publish() banks a working stint at
// the moment the conversation stops generating — the only point the daemon can
// measure one — and leaves it alone otherwise.
func TestPublishAccumulatesWorkingTime(t *testing.T) {
	d := &Daemon{entries: map[string]*entry{}}
	e := d.get_or_create("cid")

	e.item.Status = StatusReady
	e.item.Activity = ActivityIdle
	e.publish()
	require.Zero(t, e.snapshot().WorkTotal, "nothing banked before any generating")
	require.Zero(t, e.snapshot().WorkLast)

	// Start generating. Nothing is banked yet: the stint is still open, and a
	// listing adds it live from ActivitySince.
	e.item.Activity = ActivityWorking
	e.publish()
	require.Zero(t, e.snapshot().WorkTotal, "an open stint is not banked")

	// Finish it. The elapsed time lands in both figures.
	time.Sleep(5 * time.Millisecond)
	e.item.Activity = ActivityWaiting
	e.publish()
	first := e.snapshot()
	require.Positive(t, first.WorkTotal, "the finished stint is banked")
	require.Equal(t, first.WorkTotal, first.WorkLast, "the only stint is also the last one")

	// Time spent NOT generating must not accrue, no matter how many unrelated
	// republishes happen while waiting.
	time.Sleep(5 * time.Millisecond)
	e.item.Title = "some title"
	e.publish()
	require.Equal(t, first.WorkTotal, e.snapshot().WorkTotal, "waiting does not accrue")

	// A second stint adds to the total and replaces the last.
	e.item.Activity = ActivityWorking
	e.publish()
	time.Sleep(5 * time.Millisecond)
	e.item.Activity = ActivityWaiting
	e.publish()
	second := e.snapshot()
	require.Greater(t, second.WorkTotal, first.WorkTotal, "the total accumulates across stints")
	require.NotEqual(t, second.WorkTotal, second.WorkLast, "the last stint is only the most recent one")
	require.Positive(t, second.WorkLast)
}
