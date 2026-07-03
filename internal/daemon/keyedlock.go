package daemon

import "sync"

// keyedLock hands out one mutex per string key, so callers can serialize work
// that touches the same key while different keys proceed in parallel.
type keyedLock struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (k *keyedLock) get(key string) *sync.Mutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = map[string]*sync.Mutex{}
	}
	l := k.m[key]
	if l == nil {
		l = &sync.Mutex{}
		k.m[key] = l
	}
	return l
}
