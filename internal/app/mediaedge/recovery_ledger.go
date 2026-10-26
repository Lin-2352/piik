package mediaedge

import (
	"context"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// recoveryLedger counts key-frame requests by the code site that made them, one
// summary per second, so a key-frame storm shows who is asking and how often.
// It exists for diagnosis only: a Source has one when debug logging is on.
type recoveryLedger struct {
	mu       sync.Mutex
	emit     func(recoverySummary)
	now      func() time.Time
	second   int64
	requests int
	handed   int
	bySite   map[string]int
	byLayer  map[int]int
}

type recoverySummary struct {
	second    int64
	requests  int // requests made by producers
	delivered int // requests handed to the encoder after coalescing
	bySite    map[string]int
	byLayer   map[int]int
}

func newRecoveryLedger(emit func(recoverySummary), now func() time.Time) *recoveryLedger {
	return &recoveryLedger{emit: emit, now: now, bySite: map[string]int{}, byLayer: map[int]int{}}
}

// newDebugRecoveryLedger returns nil unless debug logging is on.
func newDebugRecoveryLedger() *recoveryLedger {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return nil
	}
	return newRecoveryLedger(func(s recoverySummary) {
		slog.Debug("key-frame-requests", "requests", s.requests, "delivered", s.delivered,
			"bySite", s.bySite, "byLayer", s.byLayer)
	}, time.Now)
}

func (ledger *recoveryLedger) requested(site string, layer int) {
	if ledger == nil {
		return
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.rollLocked()
	ledger.requests++
	ledger.bySite[site]++
	ledger.byLayer[layer]++
}

// delivered counts requests passed to the encoder after coalescing.
func (ledger *recoveryLedger) delivered(count int) {
	if ledger == nil || count == 0 {
		return
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	ledger.rollLocked()
	ledger.handed += count
}

// rollLocked emits the finished second, if it had any activity, and starts the next.
func (ledger *recoveryLedger) rollLocked() {
	second := ledger.now().Unix()
	if second == ledger.second {
		return
	}
	if ledger.requests > 0 && ledger.emit != nil {
		ledger.emit(recoverySummary{second: ledger.second, requests: ledger.requests,
			delivered: ledger.handed, bySite: ledger.bySite, byLayer: ledger.byLayer})
	}
	ledger.second, ledger.requests, ledger.handed = second, 0, 0
	ledger.bySite, ledger.byLayer = map[string]int{}, map[int]int{}
}

// callSite names the code that asked, skipping the request plumbing frames.
func callSite(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	for i := len(file) - 1; i >= 0; i-- {
		if file[i] == '/' || file[i] == '\\' {
			file = file[i+1:]
			break
		}
	}
	return file + ":" + strconv.Itoa(line)
}
