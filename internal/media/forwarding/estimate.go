package forwarding

import "time"

// LiveKit's send-side estimator reports this value after a reset and again as
// the result of the first probe, whatever the link really carries. It is a
// placeholder, not a measurement: adopting it pinned every connection at 100
// Mbps, so the encoder ran at its ceiling over links carrying a few Mbps.
const estimatorPlaceholderBps = 100_000_000

const (
	// A probe may raise the estimate by at most this factor at once, so one
	// optimistic result cannot jump straight to the ceiling.
	probeGrowthFactor = 2
	// The estimate never exceeds this multiple of the highest layer ceiling:
	// there is nothing to send beyond it, and probing far past it only floods the
	// pacer with padding.
	targetCeilingNumerator, targetCeilingDenominator = 3, 2

	lossThreshold   = 0.05 // fraction lost in one receiver report
	lossReports     = 2    // consecutive reports above the threshold
	lossCutInterval = time.Second
	lossCooldown    = 5 * time.Second // no probe-driven increase after a cut
	minTargetBps    = 1_000_000
)

// adoptEstimate returns the target after the estimator reports capacity, and
// whether it changed. A zero ceiling means the layer ceilings are unknown.
func adoptEstimate(current, capacity, ceiling int64, raising bool) int64 {
	if capacity <= 0 || capacity == estimatorPlaceholderBps {
		return current
	}
	if ceiling > 0 {
		capacity = min(capacity, ceiling*targetCeilingNumerator/targetCeilingDenominator)
	}
	if raising {
		if capacity <= current {
			return current
		}
		capacity = min(capacity, current*probeGrowthFactor)
	}
	return max(capacity, minTargetBps)
}

// lossGuard is the missing downward signal. The estimator only sees packets
// that were numbered and sent, so loss on the path, and NACK storms, never move
// it. Receiver reports carry the loss the viewer actually experienced: when it
// persists the target is cut in proportion, as in loss-based congestion control.
type lossGuard struct {
	bad     int
	lastCut time.Time
}

// observe takes one receiver report's fraction lost (0..1) and the current
// target. It returns the new target and whether it was cut.
func (guard *lossGuard) observe(loss float64, current int64, now time.Time) (int64, bool) {
	if loss < lossThreshold {
		guard.bad = 0
		return current, false
	}
	guard.bad++
	if guard.bad < lossReports || now.Sub(guard.lastCut) < lossCutInterval {
		return current, false
	}
	guard.bad = 0
	guard.lastCut = now
	return max(int64(float64(current)*(1-0.5*loss)), minTargetBps), true
}

// cooling reports whether a recent cut should block probe-driven increases.
func (guard *lossGuard) cooling(now time.Time) bool {
	return !guard.lastCut.IsZero() && now.Sub(guard.lastCut) < lossCooldown
}
