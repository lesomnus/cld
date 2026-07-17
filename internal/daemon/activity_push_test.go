package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lesomnus/cld/internal/tmuxx"
	"github.com/stretchr/testify/require"
)

// barrier posts fn on the entry's worker and waits for it, flushing every task
// posted before it (the mailbox is FIFO) — so an async push is observable.
func barrier(e *entry, fn func()) {
	done := make(chan struct{})
	e.mbox.post(func() {
		fn()
		close(done)
	})
	<-done
}

func TestFillActivity(t *testing.T) {
	// A tmux server pointed at a path with no server: CapturePane returns an
	// empty pane, exercising the fallback classifier without a live tmux.
	d := &Daemon{tmux: &tmuxx.Server{Socket: t.TempDir() + "/nope.sock"}}
	ctx := context.Background()

	t.Run("push entry trusts snapshot Activity and skips capture", func(t *testing.T) {
		items := []Item{{ID: "a", Name: "n", Status: StatusReady, Activity: ActivityWorking, Title: "t"}}
		d.fillActivity(ctx, items, nil)
		require.Equal(t, ActivityWorking, items[0].Activity)
	})
	t.Run("non-push entry is classified from the pane", func(t *testing.T) {
		// Empty pane (no interrupt hint) + a title => waiting.
		items := []Item{{ID: "a", Name: "n", Status: StatusReady, Title: "has title"}}
		d.fillActivity(ctx, items, nil)
		require.Equal(t, ActivityWaiting, items[0].Activity)

		// Empty pane + no title => idle.
		items = []Item{{ID: "a", Name: "n", Status: StatusReady}}
		d.fillActivity(ctx, items, nil)
		require.Equal(t, ActivityIdle, items[0].Activity)
	})
	t.Run("non-ready entry is left untouched", func(t *testing.T) {
		items := []Item{{ID: "a", Status: StatusProvisioning}}
		d.fillActivity(ctx, items, nil)
		require.Equal(t, Activity(""), items[0].Activity)
	})
	t.Run("debug captures the pane for push entries without overwriting Activity", func(t *testing.T) {
		items := []Item{{ID: "a", Name: "n", Status: StatusReady, Activity: ActivityWorking}}
		panes := map[string]string{}
		d.fillActivity(ctx, items, panes)
		require.Equal(t, ActivityWorking, items[0].Activity) // pushed value preserved
		_, captured := panes["a"]
		require.True(t, captured) // pane still captured for comparison
	})
}

func TestHandleActivity(t *testing.T) {
	d := &Daemon{entries: map[string]*entry{}}
	e := d.get_or_create("cid")
	barrier(e, func() {
		e.item.Status = StatusReady
		e.item.Name = "n"
		e.publish()
	})

	post := func(id, state string) int {
		r := httptest.NewRequest(http.MethodPost, "http://cld/activity?state="+state, nil)
		w := httptest.NewRecorder()
		d.handle_activity(w, r, id)
		barrier(e, func() {}) // flush the worker so the push is applied
		return w.Code
	}

	t.Run("valid state updates the snapshot", func(t *testing.T) {
		require.Equal(t, http.StatusNoContent, post("cid", "working"))
		require.Equal(t, ActivityWorking, e.snapshot().Activity)

		require.Equal(t, http.StatusNoContent, post("cid", "waiting"))
		require.Equal(t, ActivityWaiting, e.snapshot().Activity)
	})
	t.Run("invalid state is rejected and leaves activity unchanged", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "http://cld/activity?state=bogus", nil)
		w := httptest.NewRecorder()
		d.handle_activity(w, r, "cid")
		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Equal(t, ActivityWaiting, e.snapshot().Activity)
	})
	t.Run("unknown session id is 404", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "http://cld/activity?state=working", nil)
		w := httptest.NewRecorder()
		d.handle_activity(w, r, "does-not-exist")
		require.Equal(t, http.StatusNotFound, w.Code)
	})
	t.Run("a push to a not-ready session is accepted but ignored", func(t *testing.T) {
		e2 := d.get_or_create("cid2")
		barrier(e2, func() {
			e2.item.Status = StatusProvisioning
			e2.publish()
		})
		r := httptest.NewRequest(http.MethodPost, "http://cld/activity?state=working", nil)
		w := httptest.NewRecorder()
		d.handle_activity(w, r, "cid2")
		barrier(e2, func() {})
		require.Equal(t, http.StatusNoContent, w.Code)
		require.Equal(t, Activity(""), e2.snapshot().Activity)
	})
}
