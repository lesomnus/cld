package daemon

import "sync"

// mailbox is an unbounded FIFO of tasks drained by one worker goroutine.
// Posting never blocks the caller, so the event loop is never stalled by a
// slow container, while tasks for a single container run one at a time in the
// order they were posted.
type mailbox struct {
	mu     sync.Mutex
	q      []func()
	sig    chan struct{}
	closed bool
}

func new_mailbox() *mailbox {
	return &mailbox{sig: make(chan struct{}, 1)}
}

// post enqueues a task. It returns false if the mailbox is closed.
func (m *mailbox) post(fn func()) bool {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return false
	}
	m.q = append(m.q, fn)
	m.mu.Unlock()

	select {
	case m.sig <- struct{}{}:
	default:
	}
	return true
}

// close stops the worker after it drains the tasks already posted.
func (m *mailbox) close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.mu.Unlock()

	select {
	case m.sig <- struct{}{}:
	default:
	}
}

// run drains tasks until the mailbox is closed and empty.
func (m *mailbox) run() {
	for {
		m.mu.Lock()
		if len(m.q) == 0 {
			if m.closed {
				m.mu.Unlock()
				return
			}
			m.mu.Unlock()
			<-m.sig
			continue
		}
		fn := m.q[0]
		m.q = m.q[1:]
		m.mu.Unlock()

		fn()
	}
}
