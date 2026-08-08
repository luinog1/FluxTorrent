package main

// Process memory bounding.
//
// FT_CACHE_SIZE_MB caps only the *piece payload* held by the RAM store. On top
// of that the process holds pieces still in flight (a piece is allocated in
// full on its first write and cannot be evicted until it verifies), per-peer
// buffers, and heap that Go has not yet returned to the OS. Measured on a
// well-seeded torrent, resident memory settles at ~1.8-2.7x the configured cap,
// scaling with peer count — so a container sized "cache + a little headroom"
// gets OOM-killed mid-playback exactly when a torrent is healthy and fast.
//
// Setting a Go soft memory limit makes the GC responsible for that ceiling
// instead of the kernel: the collector works harder as the limit approaches
// rather than letting the heap grow into an OOM kill. The limit is derived from
// the cache size and clamped to the container's own cgroup limit, so raising
// the cache in Settings can never push the process past what the container is
// allowed to use.

import (
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/jodacame/fluxtorrent/internal/config"
)

const (
	mib = 1 << 20

	minHeadroom = 192 * mib // smallest allowance for peers + GC lag
	maxHeadroom = 512 * mib // beyond this, extra cache doesn't need more slack
	// Fraction of the container limit the Go heap may claim. The rest covers
	// goroutine stacks, the runtime itself and allocator overhead.
	cgroupShareNum, cgroupShareDen = 85, 100
)

// applyMemoryLimit sets the Go soft memory limit for this process unless the
// operator pinned GOMEMLIMIT explicitly.
func applyMemoryLimit(cfg config.Settings) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return // explicit operator choice wins
	}
	cacheBytes := int64(cfg.Cache.SizeMB) * mib
	limit, warn := memoryLimitFor(cacheBytes, cgroupMemLimit())
	debug.SetMemoryLimit(limit)
	if warn != "" {
		log.Printf("memory: %s", warn)
	}
	log.Printf("memory: heap limit %dMB (cache %dMB)", limit/mib, cacheBytes/mib)
}

// memoryLimitFor computes the soft heap limit from the RAM cache size, clamped
// to the container's memory limit. cgroupLimit <= 0 means "unlimited/unknown".
// The returned warning is non-empty when the configured cache leaves too little
// room inside the container for the engine to run comfortably.
func memoryLimitFor(cacheBytes, cgroupLimit int64) (int64, string) {
	if cacheBytes < 0 {
		cacheBytes = 0
	}
	headroom := cacheBytes / 4
	if headroom < minHeadroom {
		headroom = minHeadroom
	}
	if headroom > maxHeadroom {
		headroom = maxHeadroom
	}
	limit := cacheBytes + headroom

	if cgroupLimit <= 0 {
		return limit, ""
	}
	allowed := cgroupLimit / cgroupShareDen * cgroupShareNum
	if limit <= allowed {
		return limit, ""
	}
	// The cache alone doesn't fit the container with room to work: clamp the
	// heap so the GC pushes back instead of the kernel, and say so — the real
	// fix is a smaller cache or a bigger container.
	return allowed, "cache of " + strconv.FormatInt(cacheBytes/mib, 10) +
		"MB is too large for this container's " + strconv.FormatInt(cgroupLimit/mib, 10) +
		"MB limit; capping the heap at " + strconv.FormatInt(allowed/mib, 10) +
		"MB. Lower FT_CACHE_SIZE_MB or raise the container memory limit " +
		"(budget ~2.5x the cache) to avoid stalls."
}

// cgroupMemLimit returns this process's container memory limit in bytes, or 0
// when it is unlimited or cannot be determined (bare metal, unknown cgroup).
func cgroupMemLimit() int64 {
	// cgroup v2
	if v, ok := readMemLimitFile("/sys/fs/cgroup/memory.max"); ok {
		return v
	}
	// cgroup v1
	if v, ok := readMemLimitFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); ok {
		return v
	}
	return 0
}

// readMemLimitFile parses a cgroup memory limit file. "max" (v2) and the
// sentinel values v1 uses for "unlimited" both report 0.
func readMemLimitFile(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(raw))
	if s == "max" {
		return 0, true
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	// v1 reports "unlimited" as a huge sentinel (PAGE_SIZE-aligned int64 max).
	if n >= 1<<62 {
		return 0, true
	}
	return n, true
}
