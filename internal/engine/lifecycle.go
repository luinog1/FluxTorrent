package engine

// Torrent lifecycle safety.
//
// anacrolix hands out the *same* *torrent.Torrent for an infohash the client
// already knows, and its Drop() panics with "already closed" when the torrent
// was disposed of before ("it's always safe to do this" only holds for the
// first call). Two things follow, and this file provides both:
//
//   - Add, Drop and Delete must be serialized per infohash. Drop removes the
//     entry from e.managed, releases e.mu and only then closes the torrent; an
//     Add for that hash landing inside that window finds nothing in the map,
//     calls AddTorrentSpec, and gets back the very torrent that is about to be
//     closed — registering a dead torrent in e.managed, which panics the next
//     time anything sweeps it (the active-torrent limit, shutdown, a later
//     drop). Clients that add, drop and stream the same hash within seconds hit
//     this routinely, since streaming re-adds through Ensure.
//   - Every disposal must be idempotent, so the paths that legitimately race
//     (drop-after-playback, the seed enforcer, the active-torrent limit,
//     shutdown) can never call Drop twice on one torrent.

import "sync"

// markDropped reports whether this call is the first to dispose of the torrent,
// so only one caller ever reaches torrent.Drop().
func (m *managed) markDropped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dropped {
		return false
	}
	m.dropped = true
	return true
}

// dropManaged releases a managed torrent's peers and cache exactly once.
func (e *Engine) dropManaged(m *managed) {
	if m.markDropped() {
		m.t.Drop()
	}
}

// lockHash serializes lifecycle operations (add/drop/delete) for one infohash
// and returns the release function. Locks are reference-counted so the map does
// not grow with every torrent ever seen.
//
// Ordering: take this before e.mu, never the reverse. Callers already holding
// e.mu (enforceActiveLimitLocked, Close) must not take it — they rely on
// dropManaged's idempotency instead.
func (e *Engine) lockHash(hash string) func() {
	e.hashMu.Lock()
	l, ok := e.hashLocks[hash]
	if !ok {
		l = &hashLock{}
		e.hashLocks[hash] = l
	}
	l.refs++
	e.hashMu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		e.hashMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(e.hashLocks, hash)
		}
		e.hashMu.Unlock()
	}
}

// hashLock is a per-infohash mutex with a waiter count.
type hashLock struct {
	mu   sync.Mutex
	refs int
}
