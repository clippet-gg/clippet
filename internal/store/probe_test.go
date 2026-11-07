package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestProbeIsPingPutDeleteAndNothingElse pins the shape `admin doctor`'s
// `bucket` row has always had and that /setup's `Check now` now shares (§20
// erratum 297). Three calls, in that order, and NO read-back: a GET would
// change what the row proves and would cost a Class B operation per press.
func TestProbeIsPingPutDeleteAndNothingElse(t *testing.T) {
	f := NewFake()
	if err := Probe(context.Background(), f, "p/"); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	var ops []string
	for _, c := range f.CallsSnapshot() {
		ops = append(ops, strings.Fields(c)[0])
	}
	want := []string{"Ping", "PutFile", "DeletePrefix"}
	if len(ops) != len(want) {
		t.Fatalf("Probe made %v, want exactly %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("Probe made %v, want %v", ops, want)
		}
	}
	dels := f.Deletes()
	if len(dels) != 1 || !strings.HasPrefix(dels[0], "p/"+ProbeKeyFolder) {
		t.Fatalf("the probe object was %v, want one under p/%s", dels, ProbeKeyFolder)
	}
	// It is NOT under clips/, which is what keeps `admin orphans` and the
	// sweeper from ever seeing one.
	if strings.Contains(dels[0], "clips/") {
		t.Errorf("the probe object is in the clip key space: %q", dels[0])
	}
}

// TestProbeNamesTheStepAndTheClass: every failure says which step failed — the
// doctor renders a different sentence for each — and classifies itself in the
// closed §20 erratum 31 vocabulary, whichever vocabulary the underlying error
// came from.
func TestProbeNamesTheStepAndTheClass(t *testing.T) {
	cases := []struct {
		op    string
		err   error
		step  string
		class string
	}{
		{"Ping", &PingError{Class: PingDenied, Err: errors.New("403")}, ProbeStepPing, PingDenied},
		{"Ping", &PingError{Class: PingBucketMissing, Err: errors.New("no bucket")}, ProbeStepPing, PingBucketMissing},
		{"PutFile", &StorageError{Class: PublicUnavailable, Err: errors.New("refused")}, ProbeStepPut, PingUnavailable},
		{"PutFile", &StorageError{Class: PublicTimeout, Err: errors.New("idle")}, ProbeStepPut, PingTimeout},
		{"DeletePrefix", &StorageError{Class: PublicUnavailable, Err: errors.New("denied")}, ProbeStepDelete, PingUnavailable},
		{"PutFile", errors.New("something nobody classified"), ProbeStepPut, PingUnavailable},
	}
	for _, c := range cases {
		f := NewFake()
		f.FailNext(c.op, c.err)
		err := Probe(context.Background(), f, "")
		if err == nil {
			t.Fatalf("%s failing did not fail the probe", c.op)
		}
		if got := ProbeStep(err); got != c.step {
			t.Errorf("%s: step %q, want %q", c.op, got, c.step)
		}
		if got := ProbeClass(err); got != c.class {
			t.Errorf("%s: class %q, want %q", c.op, got, c.class)
		}
		// The store's own error stays reachable underneath, which is what
		// lets the doctor keep printing it (through safe()) while a page
		// prints only the class.
		if !strings.Contains(err.Error(), c.err.Error()) {
			t.Errorf("%s: the step wrapper lost the store's error: %q", c.op, err)
		}
	}
	if ProbeClass(nil) != "" || ProbeStep(nil) != "" {
		t.Errorf("nil is not a failure")
	}
}
