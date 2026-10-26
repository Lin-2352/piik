package mediaedge

import (
	"testing"
	"time"
)

func TestRecoveryLedgerSummarisesRequestsPerSecondBySite(t *testing.T) {
	now := time.Unix(100, 0)
	var lines []recoverySummary
	ledger := newRecoveryLedger(func(s recoverySummary) { lines = append(lines, s) }, func() time.Time { return now })

	ledger.requested("engine.go:186", 1)
	ledger.requested("engine.go:186", 1)
	ledger.requested("edge.go:183", -1)
	ledger.delivered(2)
	now = now.Add(1100 * time.Millisecond)
	ledger.requested("edge.go:183", -1) // next second flushes the previous one
	if len(lines) != 1 {
		t.Fatalf("flushed %d summaries, want 1", len(lines))
	}
	got := lines[0]
	if got.requests != 3 || got.delivered != 2 || got.bySite["engine.go:186"] != 2 || got.bySite["edge.go:183"] != 1 {
		t.Fatalf("summary %+v", got)
	}
	if got.byLayer[1] != 2 || got.byLayer[-1] != 1 {
		t.Fatalf("by layer %+v", got.byLayer)
	}
}

func TestRecoveryLedgerStaysSilentWhenNothingHappens(t *testing.T) {
	now := time.Unix(100, 0)
	var lines int
	ledger := newRecoveryLedger(func(recoverySummary) { lines++ }, func() time.Time { return now })
	now = now.Add(5 * time.Second)
	ledger.delivered(0)
	ledger.requested("x", 0)
	if lines != 0 {
		t.Fatalf("an empty second produced %d lines", lines)
	}
}

func TestRecoveryLedgerIsNilSafe(t *testing.T) {
	var ledger *recoveryLedger
	ledger.requested("x", 0)
	ledger.delivered(1)
}
