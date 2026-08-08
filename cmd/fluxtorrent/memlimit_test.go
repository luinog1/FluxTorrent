package main

import "testing"

func TestMemoryLimitForAddsHeadroom(t *testing.T) {
	// 1GB cache in a 1.5GB container: cache + 25% fits under 85% of the limit.
	got, warn := memoryLimitFor(1024*mib, 1536*mib)
	if want := int64(1280 * mib); got != want {
		t.Fatalf("limit = %dMB, want %dMB", got/mib, want/mib)
	}
	if warn != "" {
		t.Fatalf("unexpected warning: %s", warn)
	}
}

func TestMemoryLimitForSmallCacheGetsMinimumHeadroom(t *testing.T) {
	// A tiny cache still needs room for peers and GC lag, not cache*1.25.
	got, _ := memoryLimitFor(64*mib, 0)
	if want := int64(64*mib + minHeadroom); got != want {
		t.Fatalf("limit = %dMB, want %dMB", got/mib, want/mib)
	}
}

func TestMemoryLimitForCapsHeadroom(t *testing.T) {
	// Past the cap, extra cache doesn't buy proportionally more slack.
	got, _ := memoryLimitFor(8192*mib, 0)
	if want := int64(8192*mib + maxHeadroom); got != want {
		t.Fatalf("limit = %dMB, want %dMB", got/mib, want/mib)
	}
}

func TestMemoryLimitForClampsToContainer(t *testing.T) {
	// Cache too big for the container: clamp to 85% and warn instead of
	// letting the kernel OOM-kill the process mid-playback.
	got, warn := memoryLimitFor(4096*mib, 1536*mib)
	if want := int64(1536 * mib / 100 * 85); got != want {
		t.Fatalf("limit = %dMB, want %dMB", got/mib, want/mib)
	}
	if warn == "" {
		t.Fatal("expected a warning when the cache exceeds the container limit")
	}
}

func TestMemoryLimitForUnlimitedCgroup(t *testing.T) {
	if got, warn := memoryLimitFor(512*mib, 0); got != 512*mib+minHeadroom || warn != "" {
		t.Fatalf("limit = %dMB warn=%q", got/mib, warn)
	}
}

func TestReadMemLimitFileMissing(t *testing.T) {
	if _, ok := readMemLimitFile("/nonexistent/memory.max"); ok {
		t.Fatal("missing file should report not-ok")
	}
}
