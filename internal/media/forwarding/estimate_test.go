package forwarding

import (
	"testing"
	"time"
)

func TestAdoptEstimateIgnoresPlaceholderAndLimitsGrowth(t *testing.T) {
	const ceiling = 38_000_000
	if got := adoptEstimate(19_500_000, estimatorPlaceholderBps, ceiling, true); got != 19_500_000 {
		t.Fatalf("placeholder adopted: %d", got)
	}
	if got := adoptEstimate(19_500_000, estimatorPlaceholderBps, ceiling, false); got != 19_500_000 {
		t.Fatalf("placeholder adopted by the congestion callback: %d", got)
	}
	// One optimistic probe at most doubles the estimate, and never exceeds 1.5x the ceiling.
	if got := adoptEstimate(10_000_000, 90_000_000, ceiling, true); got != 20_000_000 {
		t.Fatalf("growth not limited: %d", got)
	}
	if got := adoptEstimate(40_000_000, 90_000_000, ceiling, true); got != 57_000_000 {
		t.Fatalf("ceiling not applied: %d", got)
	}
	// A probe never lowers the estimate; the congestion callback may.
	if got := adoptEstimate(30_000_000, 20_000_000, ceiling, true); got != 30_000_000 {
		t.Fatalf("probe lowered the estimate: %d", got)
	}
	if got := adoptEstimate(30_000_000, 12_000_000, ceiling, false); got != 12_000_000 {
		t.Fatalf("congestion capacity ignored: %d", got)
	}
	if got := adoptEstimate(30_000_000, 100_000, ceiling, false); got != minTargetBps {
		t.Fatalf("floor not applied: %d", got)
	}
	if got := adoptEstimate(5_000_000, 0, ceiling, false); got != 5_000_000 {
		t.Fatalf("zero capacity changed the estimate: %d", got)
	}
}

func TestLossGuardCutsOnlyOnPersistentLoss(t *testing.T) {
	var guard lossGuard
	now := time.Unix(100, 0)
	current := int64(20_000_000)
	if next, cut := guard.observe(0.5, current, now); cut || next != current {
		t.Fatal("one report must not cut")
	}
	if _, cut := guard.observe(0.01, current, now.Add(time.Second)); cut {
		t.Fatal("a clean report must reset the streak")
	}
	guard.observe(0.4, current, now.Add(2*time.Second))
	next, cut := guard.observe(0.4, current, now.Add(3*time.Second))
	if !cut || next != 16_000_000 {
		t.Fatalf("40%% loss: cut=%v next=%d, want 16000000", cut, next)
	}
	if !guard.cooling(now.Add(4*time.Second)) || guard.cooling(now.Add(9*time.Second)) {
		t.Fatal("cooldown window is wrong")
	}
	// Cuts are rate limited and bounded below.
	guard.observe(0.9, next, now.Add(3*time.Second+100*time.Millisecond))
	if _, cut := guard.observe(0.9, next, now.Add(3*time.Second+200*time.Millisecond)); cut {
		t.Fatal("a second cut inside the interval")
	}
	floor, _ := (&lossGuard{bad: 5}).observe(1, 1_200_000, now)
	if floor != minTargetBps {
		t.Fatalf("floor = %d", floor)
	}
}

func TestProbeCapacityUsesDeliveredRateWhenFinalizeReturnsThePlaceholder(t *testing.T) {
	if got := probeCapacity(estimatorPlaceholderBps, 34_900_000); got != 34_900_000 {
		t.Fatalf("placeholder not replaced: %d", got)
	}
	if got := probeCapacity(0, 12_000_000); got != 12_000_000 {
		t.Fatalf("missing estimate not replaced: %d", got)
	}
	if got := probeCapacity(25_000_000, 34_900_000); got != 25_000_000 {
		t.Fatalf("a real estimate was overridden: %d", got)
	}
	// From the frozen 19.5 Mbps start, two clean probes reach the ceiling cap.
	target := int64(19_512_000)
	for range 3 {
		target = adoptEstimate(target, probeCapacity(estimatorPlaceholderBps, 60_000_000), 38_000_000, true)
	}
	if target != 57_000_000 {
		t.Fatalf("target did not grow to the ceiling cap: %d", target)
	}
}
