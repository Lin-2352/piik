package forwarding

import (
	"testing"
	"time"

	"github.com/pion/rtp"
)

// traceClock is a hand-driven clock so buckets are exact.
type traceClock struct{ now time.Time }

func (c *traceClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestTrace(clock *traceClock, emitted *[]traceBucket) *joinTrace {
	return newJoinTrace("conn", func(b traceBucket) { *emitted = append(*emitted, b) }, func() time.Time { return clock.now })
}

func traceHeader(seq uint16, ts uint32, marker bool) *rtp.Header {
	return &rtp.Header{SequenceNumber: seq, Timestamp: ts, Marker: marker}
}

func TestJoinTraceCountsMediaProbeFramesAndBytesPerBucket(t *testing.T) {
	clock := &traceClock{now: time.Unix(100, 0)}
	var out []traceBucket
	trace := newTestTrace(clock, &out)
	trace.observe(traceHeader(1, 1000, false), 1000, false, 3)
	trace.observe(traceHeader(2, 1000, true), 500, false, 5)
	trace.observe(traceHeader(3, 1000, false), 1200, true, 9) // probe padding keeps the sequence going
	clock.advance(120 * time.Millisecond)
	trace.observe(traceHeader(4, 2000, true), 300, false, 1) // next bucket flushes the first
	if len(out) != 1 {
		t.Fatalf("flushed %d buckets, want 1", len(out))
	}
	got := out[0]
	if got.index != 0 || got.media != 2 || got.probe != 1 || got.frames != 1 || got.bytes != 1500 || got.probeBytes != 1200 || got.maxPending != 9 {
		t.Fatalf("bucket %+v", got)
	}
}

func TestJoinTraceFlagsSequenceGapsAndTimestampRegressions(t *testing.T) {
	clock := &traceClock{now: time.Unix(100, 0)}
	var out []traceBucket
	trace := newTestTrace(clock, &out)
	trace.observe(traceHeader(10, 5000, true), 100, false, 0)
	trace.observe(traceHeader(12, 5750, true), 100, false, 0) // one packet missing
	trace.observe(traceHeader(13, 4000, true), 100, false, 0) // timestamp went backwards
	clock.advance(150 * time.Millisecond)
	trace.observe(traceHeader(14, 4750, true), 100, false, 0)
	got := out[0]
	if got.seqGaps != 1 || got.tsBack != 1 {
		t.Fatalf("seqGaps %d tsBack %d, want 1 and 1: %+v", got.seqGaps, got.tsBack, got)
	}
}

func TestJoinTraceReportsTheLargestFrameInPackets(t *testing.T) {
	clock := &traceClock{now: time.Unix(100, 0)}
	var out []traceBucket
	trace := newTestTrace(clock, &out)
	for seq := uint16(1); seq <= 300; seq++ {
		trace.observe(traceHeader(seq, 9000, seq == 300), 1100, false, 0) // one 300-packet key frame
	}
	trace.observe(traceHeader(301, 9750, true), 100, false, 0)
	clock.advance(150 * time.Millisecond)
	trace.observe(traceHeader(302, 10500, true), 100, false, 0)
	if out[0].maxFramePackets != 300 {
		t.Fatalf("largest frame %d packets, want 300: %+v", out[0].maxFramePackets, out[0])
	}
}

func TestJoinTraceStopsAfterTheJoinWindow(t *testing.T) {
	clock := &traceClock{now: time.Unix(100, 0)}
	var out []traceBucket
	trace := newTestTrace(clock, &out)
	trace.observe(traceHeader(1, 1, true), 100, false, 0)
	clock.advance(joinTraceWindow + time.Second)
	trace.observe(traceHeader(2, 2, true), 100, false, 0)
	count := len(out)
	clock.advance(time.Second)
	trace.observe(traceHeader(3, 3, true), 100, false, 0)
	if len(out) != count || !trace.finished() {
		t.Fatalf("trace kept recording after its window: %d buckets", len(out))
	}
}
