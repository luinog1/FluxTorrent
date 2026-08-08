package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/jodacame/fluxtorrent/internal/config"
)

// newTestEngine builds an engine backed by a temp store, with networking off so
// the test never reaches a tracker or the DHT.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := store.Get()
	cfg.Net.DHT, cfg.Net.BTPort = false, 0
	cfg.Cache.Mode, cfg.Cache.SizeMB, cfg.Cache.Path = "ram", 64, t.TempDir()
	cfg.Compressed.Reject = false
	if err := store.Put(cfg); err != nil {
		t.Fatalf("put settings: %v", err)
	}
	eng, err := New(store)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { eng.Close(); _ = store.Close() })
	return eng
}

// serveTorrent publishes a .torrent over HTTP whose metadata is embedded, so
// Add resolves it without peers — the offline stand-in for an indexer link.
func serveTorrent(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	payload := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(payload, make([]byte, 512<<10), 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(payload); err != nil {
		t.Fatalf("build info: %v", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("marshal info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	raw, err := bencode.Marshal(mi)
	if err != nil {
		t.Fatalf("marshal metainfo: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestConcurrentAddOfUnresolvableHash reproduces the production crash. A client
// retrying a link whose metadata never resolves lands several Add calls on one
// infohash at once; AddTorrentSpec hands every one of them the *same* torrent,
// and each closes it when its context expires. The first close wins and the
// rest panic anacrolix with "already closed" — observed in the wild as repeated
// `http: panic serving ... already closed` at the metadata-timeout Drop, all
// reporting the identical torrent pointer.
func TestConcurrentAddOfUnresolvableHash(t *testing.T) {
	eng := newTestEngine(t)

	// No DHT, no trackers, no peers: metadata can never arrive, so every call
	// takes the ctx.Done() branch that disposes of the torrent.
	const hash = "0123456789abcdef0123456789abcdef01234567"
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := eng.Add(ctx, hash); err == nil {
				t.Error("add of an unresolvable hash should fail, not succeed")
			}
		}()
	}
	wg.Wait()

	// The failed adds must leave nothing behind.
	eng.mu.Lock()
	n := len(eng.managed)
	eng.mu.Unlock()
	if n != 0 {
		t.Fatalf("managed retained %d entries after failed adds, want 0", n)
	}
}

// TestAddDropChurnDoesNotPanic exercises the other door onto the same
// corruption — add, drop and re-add of one hash interleaved with the
// active-torrent sweep, the sequence a player produces when it adds a torrent,
// drops it and immediately streams it (which re-adds through Ensure).
func TestAddDropChurnDoesNotPanic(t *testing.T) {
	eng := newTestEngine(t)
	link := serveTorrent(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := eng.Add(ctx, link)
	if err != nil {
		t.Fatalf("seed add: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for n := 0; n < 150; n++ {
			if _, err := eng.Add(ctx, link); err != nil && ctx.Err() == nil {
				t.Errorf("add: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; n < 150; n++ {
			_ = eng.Drop(res.Hash)
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; n < 150; n++ {
			eng.mu.Lock()
			eng.enforceActiveLimitLocked(0) // force every idle torrent out
			eng.mu.Unlock()
		}
	}()
	wg.Wait()
}

// TestMarkDroppedOnlyOnce is the invariant the sweep relies on: exactly one
// caller may reach torrent.Drop() for a given managed torrent.
func TestMarkDroppedOnlyOnce(t *testing.T) {
	m := &managed{}
	var wg sync.WaitGroup
	var firsts int64
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.markDropped() {
				mu.Lock()
				firsts++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firsts != 1 {
		t.Fatalf("markDropped returned true %d times, want exactly 1", firsts)
	}
}

// TestLockHashReleasesEntries guards the per-hash lock's bookkeeping: the map
// must not grow with every torrent the server ever sees.
func TestLockHashReleasesEntries(t *testing.T) {
	eng := newTestEngine(t)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				eng.lockHash("deadbeef")()
			}
		}()
	}
	wg.Wait()

	eng.hashMu.Lock()
	n := len(eng.hashLocks)
	eng.hashMu.Unlock()
	if n != 0 {
		t.Fatalf("hashLocks retained %d entries, want 0", n)
	}
}
