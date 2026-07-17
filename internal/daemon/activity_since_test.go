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
