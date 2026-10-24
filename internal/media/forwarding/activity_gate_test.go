package forwarding

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestActivityGateAdmitsOneEvaluationPerInterval(t *testing.T) {
	var gate activityGate
	base := time.Unix(100, 0)
	every := 10 * time.Millisecond
	if !gate.due(base, every) {
		t.Fatal("the first call must run")
	}
	for _, offset := range []time.Duration{0, time.Millisecond, 9 * time.Millisecond} {
		if gate.due(base.Add(offset), every) {
			t.Fatalf("a call %v after the last run must be skipped", offset)
		}
	}
	if !gate.due(base.Add(10*time.Millisecond), every) {
		t.Fatal("a call one interval after the last run must run")
	}
	if gate.due(base.Add(15*time.Millisecond), every) {
		t.Fatal("the interval restarts from the last run, not from the first")
	}
}

func TestActivityGateRunsOnceAcrossGoroutines(t *testing.T) {
	var gate activityGate
	var ran atomic.Int32
	var wait sync.WaitGroup
	now := time.Unix(200, 0)
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if gate.due(now, time.Second) {
				ran.Add(1)
			}
		}()
	}
	wait.Wait()
	if ran.Load() != 1 {
		t.Fatalf("%d goroutines ran the evaluation, want exactly 1", ran.Load())
	}
}
