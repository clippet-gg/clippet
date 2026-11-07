package transcode

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCPUQuota reads both cgroup layouts, and the two spellings of "no quota".
// The whole point of erratum 215 is that this file is the only place that knows
// how many CPUs the container really has, so every shape it can arrive in is
// pinned here.
func TestCPUQuota(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		cpus  int
		ok    bool
	}{
		{name: "v2 one cpu", files: map[string]string{"cpu.max": "100000 100000\n"}, cpus: 1, ok: true},
		{name: "v2 four cpus", files: map[string]string{"cpu.max": "400000 100000"}, cpus: 4, ok: true},
		// cpus: 1.5 — half a core is still a whole thread.
		{name: "v2 fractional rounds up", files: map[string]string{"cpu.max": "150000 100000"}, cpus: 2, ok: true},
		{name: "v2 unlimited", files: map[string]string{"cpu.max": "max 100000"}},
		{name: "v2 malformed", files: map[string]string{"cpu.max": "nonsense"}},
		{name: "v1 under cpu/", files: map[string]string{
			"cpu/cpu.cfs_quota_us": "200000", "cpu/cpu.cfs_period_us": "100000"}, cpus: 2, ok: true},
		{name: "v1 at the root", files: map[string]string{
			"cpu.cfs_quota_us": "800000", "cpu.cfs_period_us": "100000"}, cpus: 8, ok: true},
		{name: "v1 unlimited", files: map[string]string{
			"cpu/cpu.cfs_quota_us": "-1", "cpu/cpu.cfs_period_us": "100000"}},
		{name: "nothing mounted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for name, body := range tc.files {
				path := filepath.Join(root, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cpus, ok := cpuQuota(root)
			if ok != tc.ok || (ok && cpus != tc.cpus) {
				t.Fatalf("cpuQuota = %d, %v; want %d, %v", cpus, ok, tc.cpus, tc.ok)
			}
		})
	}
}

// TestResolveThreads: the operator wins, then the quota, then the core count —
// and the quota never exceeds the cores, because a quota larger than the machine
// is a misconfiguration and not a licence to spawn threads for cores that are
// not there.
func TestResolveThreads(t *testing.T) {
	quota := func(t *testing.T, body string) string {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "cpu.max"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}
	sixteen := func() int { return 16 }

	if got := resolveThreads(3, quota(t, "100000 100000"), sixteen); got != 3 {
		t.Errorf("TRANSCODE_THREADS=3 → %d, want 3 (the operator's number wins over the quota)", got)
	}
	if got := resolveThreads(0, quota(t, "100000 100000"), sixteen); got != 1 {
		t.Errorf("auto under cpus:1.0 on a 16-core host → %d, want 1 — the whole of erratum 215", got)
	}
	if got := resolveThreads(0, quota(t, "400000 100000"), sixteen); got != 4 {
		t.Errorf("auto under cpus:4.0 → %d, want 4", got)
	}
	if got := resolveThreads(0, quota(t, "max 100000"), sixteen); got != 16 {
		t.Errorf("auto with no quota → %d, want the core count 16", got)
	}
	if got := resolveThreads(0, t.TempDir(), sixteen); got != 16 {
		t.Errorf("auto with no cgroup files → %d, want the core count 16", got)
	}
	// A quota bigger than the machine: the cores bound it.
	if got := resolveThreads(0, quota(t, "3200000 100000"), sixteen); got != 16 {
		t.Errorf("auto under a 32-cpu quota on a 16-core host → %d, want 16", got)
	}
	if got := resolveThreads(0, t.TempDir(), func() int { return 0 }); got != 1 {
		t.Errorf("auto on a machine reporting no cores → %d, want 1 (0 means 'you decide' to ffmpeg)", got)
	}
	// The exported form must not panic on a host with no cgroups at all (macOS,
	// Windows, a bare VM) — it is called at boot on every platform.
	if got := ResolveThreads(0); got < 1 {
		t.Errorf("ResolveThreads(0) on this host → %d, want at least 1", got)
	}
}
