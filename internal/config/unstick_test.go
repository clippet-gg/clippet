package config

import (
	"strings"
	"testing"
	"time"
)

// JOBS_UNSTICK_AFTER and the schedule it resolves to (§20 erratum 230). The
// variable has three states no other duration in §4 has all three of — unset
// (a default computed from another variable), 0 (a deliberate off) and a value
// that may be RAISED rather than obeyed — so each of them is pinned here.

func TestUnstickAfterDefaultsToTheHardTimeoutPlusTheMargin(t *testing.T) {
	c := mustLoad(t, nil)
	want := c.Transcode.HardTimeout + UnstickMargin
	if c.JobsUnstickAfter != want {
		t.Fatalf("JobsUnstickAfter = %s with the variable unset, want TRANSCODE_HARD_TIMEOUT + %s = %s",
			c.JobsUnstickAfter, UnstickMargin, want)
	}
	// And it FOLLOWS the hard timeout, which is the whole reason it is not a
	// literal in compose.yaml.
	c = mustLoad(t, map[string]string{"TRANSCODE_HARD_TIMEOUT": "30m"})
	if want := 30*time.Minute + UnstickMargin; c.JobsUnstickAfter != want {
		t.Fatalf("JobsUnstickAfter = %s with a 30m hard timeout, want %s", c.JobsUnstickAfter, want)
	}
	p := c.UnstickPlan()
	if !p.On() || p.After != 30*time.Minute+UnstickMargin || p.Raised() {
		t.Fatalf("plan = %+v, want the default on and unraised", p)
	}
}

func TestUnstickPlan(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		set, hardTimeout                 string
		wantOn, wantRaise, wantUnderHard bool
		wantAfter                        time.Duration
		wantEvery                        time.Duration
	}{
		{name: "off by choice", set: "0", wantOn: false},
		{name: "off with a unit", set: "0s", wantOn: false},
		{
			// The shipped default: past the hard timeout by the margin, so
			// nothing a live encode could still be inside — anywhere — is
			// repaired. A quarter of it is past the tick ceiling.
			name: "the default", hardTimeout: "6h",
			wantOn: true, wantAfter: 6*time.Hour + UnstickMargin, wantEvery: maxUnstickEvery,
		},
		{
			name: "an hour on a 30m timeout", set: "1h", hardTimeout: "30m",
			wantOn: true, wantAfter: time.Hour, wantEvery: maxUnstickEvery,
		},
		{
			// UNDER the hard timeout: obeyed, and flagged. This process never
			// repairs its own live job whatever the age, so the flag is about a
			// SECOND process sharing the database.
			name: "under the hard timeout", set: "5m", hardTimeout: "6h",
			wantOn: true, wantUnderHard: true, wantAfter: 5 * time.Minute, wantEvery: 75 * time.Second,
		},
		{
			// The E2E's shape: seconds, so a probe instance can prove the loop
			// works — and the alive-job exclusion — inside one run.
			name: "the sanity floor", set: "10s", hardTimeout: "20s",
			wantOn: true, wantRaise: true, wantUnderHard: false, wantAfter: MinUnstickAfter, wantEvery: MinUnstickAfter / 4,
		},
		{
			name: "exactly the floor", set: "30s", hardTimeout: "5m",
			wantOn: true, wantUnderHard: true, wantAfter: MinUnstickAfter, wantEvery: MinUnstickAfter / 4,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			over := map[string]string{}
			if tc.set != "" {
				over["JOBS_UNSTICK_AFTER"] = tc.set
			}
			if tc.hardTimeout != "" {
				over["TRANSCODE_HARD_TIMEOUT"] = tc.hardTimeout
			}
			p := mustLoad(t, over).UnstickPlan()
			if p.On() != tc.wantOn {
				t.Fatalf("plan %+v: On() = %v, want %v", p, p.On(), tc.wantOn)
			}
			if !tc.wantOn {
				if p.After != 0 || p.Every != 0 || p.Raised() || p.UnderHardTimeout() {
					t.Fatalf("plan %+v: an off plan must resolve to no age, no tick and no warning", p)
				}
				return
			}
			if p.After != tc.wantAfter {
				t.Errorf("After = %s, want %s", p.After, tc.wantAfter)
			}
			if p.Every != tc.wantEvery {
				t.Errorf("Every = %s, want %s", p.Every, tc.wantEvery)
			}
			if p.Raised() != tc.wantRaise {
				t.Errorf("Raised() = %v, want %v (configured %s, floor %s)", p.Raised(), tc.wantRaise, p.Configured, p.Floor)
			}
			if p.UnderHardTimeout() != tc.wantUnderHard {
				t.Errorf("UnderHardTimeout() = %v, want %v (after %s, hard timeout %s)",
					p.UnderHardTimeout(), tc.wantUnderHard, p.After, p.HardTimeout)
			}
			if p.Every <= 0 {
				t.Error("a live plan with a zero tick would panic time.NewTicker")
			}
			if p.After < MinUnstickAfter {
				t.Errorf("After = %s is under the sanity floor %s", p.After, MinUnstickAfter)
			}
		})
	}
}

func TestUnstickAfterRefusesNonsense(t *testing.T) {
	for _, v := range []string{"banana", "-1h", "6", "5 m"} {
		err := loadErr(t, map[string]string{"JOBS_UNSTICK_AFTER": v})
		if !strings.Contains(err.Error(), "JOBS_UNSTICK_AFTER") {
			t.Errorf("JOBS_UNSTICK_AFTER=%q: error does not name the variable: %v", v, err)
		}
	}
}
