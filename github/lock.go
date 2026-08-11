package github

import "sync"

// keyedMutex serializes operations per key without a global lock. Storage
// moving from quack's shared Postgres to this extension's own SQLite drops
// whatever incidental serialization the shared connection/transaction
// surface provided (design doc Risk 2) - tryMergeStandingIntent's
// read-verdict-act sequence spans a live GitHub API call between reading
// the merge intent and deleting it, so it can't be a single SQL
// transaction; this closes the same race at the call-site level instead,
// scoped to one chat so unrelated chats never wait on each other.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// Lock blocks until key's lock is held, returning the func that releases it.
func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*sync.Mutex{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &sync.Mutex{}
		k.locks[key] = l
	}
	k.mu.Unlock()

	l.Lock()
	return l.Unlock
}
