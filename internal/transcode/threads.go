package transcode

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Thread pinning (DESIGN.md §20 erratum 215).
//
// `nproc` inside a container reports every core the HOST has, whatever
// `cpus:` says: a CFS quota is a share of time, not a cpuset, and nothing in
// /proc changes when the quota does. So on the compose default (`cpus: 1.0` on
// a sixteen-core host) x264 sized its frame-thread pool for sixteen cores and
// was then throttled to one — measured 25 % slower than the same encode with
// `-threads 1`, and the output was 2–5 % LARGER as well (more frame threads
// means more slices and less lookahead across them).
//
// So every encode carries `-threads N` and `-filter_threads N`, and N is the
// quota unless the operator said otherwise. Reading the quota beats adding a
// variable an operator would have to keep in step with `APP_CPUS` by hand,
// which is exactly the kind of pair that drifts; TRANSCODE_THREADS exists for
// the operator who knows better than the arithmetic.
//
// The one measured regression is a source already at the output size on eight
// cores, where pinning cost 18–20 %: x264's frame threads are the only
// parallelism a no-op scale leaves it. That is why the quota is used as the
// number rather than as a cap, and why the variable is here.

// cgroupRoot is where a Linux container's own cgroup files are mounted. It is
// a variable so the test can point at a directory it wrote.
const cgroupRoot = "/sys/fs/cgroup"

// ResolveThreads is the `-threads` value of every encode: the operator's
// TRANSCODE_THREADS when they set one, else the container's CPU quota, else
// the machine's core count.
//
// It never returns less than one: `-threads 0` means "decide for yourself" to
// ffmpeg, which is precisely the behaviour this exists to replace.
func ResolveThreads(configured int) int {
	return resolveThreads(configured, cgroupRoot, runtime.NumCPU)
}

// resolveThreads is ResolveThreads with its two environment reads injected.
func resolveThreads(configured int, root string, numCPU func() int) int {
	if configured > 0 {
		return configured
	}
	cores := numCPU()
	if cores < 1 {
		cores = 1
	}
	if q, ok := cpuQuota(root); ok {
		return min(q, cores)
	}
	return cores
}

// cpuQuota reads the container's CFS quota as a whole number of CPUs, ROUNDED
// UP: half a core is one thread, because zero threads is not a number ffmpeg
// takes and a fractional quota still gets a whole core's worth of work done in
// bursts.
//
// cgroup v2 (`cpu.max`) is read first because it is what Docker Desktop's WSL2
// kernel, every current systemd host and every modern distro mount; v1's pair
// (`cpu.cfs_quota_us` / `cpu.cfs_period_us`) is the fallback for older hosts.
// "max" (v2) and -1 (v1) both mean "no quota", which is not a failure to read —
// it is the honest answer that the core count is the bound.
func cpuQuota(root string) (int, bool) {
	if q, p, ok := readCPUMax(filepath.Join(root, "cpu.max")); ok {
		return quotaCPUs(q, p)
	}
	q, ok1 := readNumberFile(filepath.Join(root, "cpu", "cpu.cfs_quota_us"))
	p, ok2 := readNumberFile(filepath.Join(root, "cpu", "cpu.cfs_period_us"))
	if !ok1 || !ok2 {
		// A v1 host that mounts the controller at the root rather than under a
		// `cpu/` subdirectory (a container of its own cgroup namespace).
		q, ok1 = readNumberFile(filepath.Join(root, "cpu.cfs_quota_us"))
		p, ok2 = readNumberFile(filepath.Join(root, "cpu.cfs_period_us"))
	}
	if !ok1 || !ok2 {
		return 0, false
	}
	return quotaCPUs(q, p)
}

// readCPUMax parses cgroup v2's `cpu.max`: "<quota> <period>", where quota is
// either a number of microseconds or the word `max`.
func readCPUMax(path string) (quota, period int64, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return 0, 0, false
	}
	if fields[0] == "max" {
		return -1, 0, true
	}
	q, err1 := strconv.ParseInt(fields[0], 10, 64)
	p, err2 := strconv.ParseInt(fields[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return q, p, true
}

// readNumberFile reads one whole number from a cgroup v1 file.
func readNumberFile(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// quotaCPUs turns a quota/period pair into whole CPUs. ok=false means "no
// quota is in force", which every caller reads as "use the core count".
func quotaCPUs(quota, period int64) (int, bool) {
	if quota <= 0 || period <= 0 {
		return 0, false
	}
	cpus := (quota + period - 1) / period // ceil
	if cpus < 1 {
		cpus = 1
	}
	return int(cpus), true
}
