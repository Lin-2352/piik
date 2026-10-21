package mediaedge

import (
	"testing"
	"time"

	"github.com/TNTcraftHIM/Piik/internal/media/forwarding"
)

type stubConsumer struct{}

func (stubConsumer) CurrentSource() *forwarding.Source      { return nil }
func (stubConsumer) ReplaceSource(*forwarding.Source) error { return nil }

func TestBudgetHoldDampsEstimateNoise(t *testing.T) {
	now := time.Unix(0, 0)
	source := &Source{now: func() time.Time { return now }}
	consumer := &stubConsumer{}
	const ceiling = 40_000_000
	step := func(raw uint32, advance time.Duration) uint32 {
		now = now.Add(advance)
		return source.holdBudget(consumer, raw, ceiling)
	}
	if got := step(ceiling, 0); got != ceiling {
		t.Fatalf("first estimate = %d", got)
	}
	// Jitter within one 5% step never moves the target.
	for _, raw := range []uint32{39_100_000, 38_500_000, 39_900_000} {
		if got := step(raw, 100*time.Millisecond); got != 38_000_000 && got != ceiling {
			t.Fatalf("jitter moved budget to %d", got)
		}
	}
	// A modest fall applies only after it persists for a second.
	now = now.Add(5 * time.Second)
	source.heldBudgets[consumer] = heldBudget{value: ceiling}
	if got := step(34_000_000, 0); got != ceiling {
		t.Fatalf("transient fall applied at once: %d", got)
	}
	if got := step(34_000_000, 1100*time.Millisecond); got != 34_000_000 {
		t.Fatalf("sustained fall not applied: %d", got)
	}
	// A large fall is congestion and applies immediately.
	if got := step(10_000_000, 0); got != 10_000_000 {
		t.Fatalf("congestion fall delayed: %d", got)
	}
	// A rise waits for two seconds of headroom; a dip restarts the wait.
	if got := step(20_000_000, 1500*time.Millisecond); got != 10_000_000 {
		t.Fatalf("rise applied early: %d", got)
	}
	if got := step(20_000_000, 2100*time.Millisecond); got != 20_000_000 {
		t.Fatalf("sustained rise not applied: %d", got)
	}
}
