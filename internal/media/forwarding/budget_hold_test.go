package forwarding

import (
	"testing"
	"time"
)

func TestBudgetHoldIgnoresShortDipsAndFollowsSustainedChange(t *testing.T) {
	var hold budgetHold
	start := time.Unix(100, 0)
	if got := hold.apply(8_000_000, start); got != 8_000_000 {
		t.Fatalf("first estimate = %d", got)
	}
	// A dip that recovers inside the lower wait never reaches the layer decision.
	if got := hold.apply(6_000_000, start.Add(100*time.Millisecond)); got != 8_000_000 {
		t.Fatalf("short dip applied: %d", got)
	}
	if got := hold.apply(8_000_000, start.Add(500*time.Millisecond)); got != 8_000_000 {
		t.Fatalf("recovery = %d", got)
	}
	// A sustained fall applies after the lower wait.
	hold.apply(6_000_000, start.Add(time.Second))
	if got := hold.apply(6_000_000, start.Add(2*time.Second+time.Millisecond)); got != 6_000_000 {
		t.Fatalf("sustained fall = %d", got)
	}
	// A large fall is congestion and applies at once.
	if got := hold.apply(1_000_000, start.Add(2100*time.Millisecond)); got != 1_000_000 {
		t.Fatalf("large fall = %d", got)
	}
	// A rise waits for sustained headroom.
	hold.apply(1_200_000, start.Add(3*time.Second))
	if got := hold.apply(1_200_000, start.Add(4*time.Second)); got != 1_000_000 {
		t.Fatalf("rise applied early: %d", got)
	}
	if got := hold.apply(1_200_000, start.Add(5*time.Second+time.Millisecond)); got != 1_200_000 {
		t.Fatalf("sustained rise = %d", got)
	}
}

func TestTopLayerFloorDivisorDefaultsToHalf(t *testing.T) {
	var source Source
	if source.TopLayerFloorDivisor() != 2 {
		t.Fatalf("default divisor = %d", source.TopLayerFloorDivisor())
	}
	source.SetTopLayerFloorDivisor(8)
	if source.TopLayerFloorDivisor() != 8 {
		t.Fatalf("divisor = %d", source.TopLayerFloorDivisor())
	}
}
