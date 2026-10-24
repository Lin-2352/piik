package forwarding

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/rtp"
)

// joinTrace summarises what one connection actually put on the wire during its
// first seconds, in 100 ms buckets. It exists to diagnose join-time stalls: a
// viewer that receives packets but completes no frame shows up as media bytes
// with no frames, a sequence gap, a timestamp regression or a huge key frame.
// It records only when debug logging is on and stops after joinTraceWindow.
const (
	joinTraceBucket = 100 * time.Millisecond
	joinTraceWindow = 6 * time.Second
)

type traceBucket struct {
	index                       int // bucket start, in 100 ms units since the first packet
	media, probe                int
	bytes, probeBytes           int
	frames, seqGaps, tsBack     int
	rtx                         int
	maxFramePackets, maxPending int
	maxTSJump                   uint32
}

type joinTrace struct {
	mu       sync.Mutex
	id       string
	emit     func(traceBucket)
	now      func() time.Time
	started  bool
	start    time.Time
	done     bool
	current  traceBucket
	haveSeq  bool
	lastSeq  uint16
	haveTS   bool
	lastTS   uint32
	frameTS  uint32
	framePkt int
}

func newJoinTrace(id string, emit func(traceBucket), now func() time.Time) *joinTrace {
	return &joinTrace{id: id, emit: emit, now: now}
}

// newDebugJoinTrace returns nil unless debug logging is on, so the normal path
// pays one nil check per packet.
func newDebugJoinTrace(id string) *joinTrace {
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		return nil
	}
	return newJoinTrace(id, func(b traceBucket) {
		slog.Debug("join-trace", "connectionId", id, "ms", b.index*int(joinTraceBucket/time.Millisecond),
			"media", b.media, "probe", b.probe, "bytes", b.bytes, "probeBytes", b.probeBytes,
			"frames", b.frames, "maxFramePackets", b.maxFramePackets, "seqGaps", b.seqGaps,
			"tsBack", b.tsBack, "rtx", b.rtx, "maxTSJump", b.maxTSJump, "maxPending", b.maxPending)
	}, time.Now)
}

func (trace *joinTrace) finished() bool {
	if trace == nil {
		return true
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return trace.done
}

// observe records one packet as it is sent. pending is the pacer queue depth.
func (trace *joinTrace) observe(header *rtp.Header, payload int, probe bool, pending int) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.done {
		return
	}
	now := trace.now()
	if !trace.started {
		trace.started, trace.start = true, now
	}
	elapsed := now.Sub(trace.start)
	index := int(elapsed / joinTraceBucket)
	if index != trace.current.index {
		trace.flushLocked()
		trace.current = traceBucket{index: index}
	}
	if elapsed >= joinTraceWindow {
		trace.flushLocked()
		trace.done = true
		return
	}
	bucket := &trace.current
	if pending > bucket.maxPending {
		bucket.maxPending = pending
	}
	if trace.haveSeq && header.SequenceNumber != trace.lastSeq+1 {
		bucket.seqGaps++
	}
	trace.haveSeq, trace.lastSeq = true, header.SequenceNumber
	if probe {
		bucket.probe++
		bucket.probeBytes += payload
		return
	}
	bucket.media++
	bucket.bytes += payload
	if trace.haveTS && header.Timestamp != trace.frameTS {
		// A new frame starts: close the previous frame's packet count.
		if trace.framePkt > bucket.maxFramePackets {
			bucket.maxFramePackets = trace.framePkt
		}
		trace.framePkt = 0
		if delta := int32(header.Timestamp - trace.lastTS); delta < 0 {
			bucket.tsBack++
		} else if uint32(delta) > bucket.maxTSJump {
			bucket.maxTSJump = uint32(delta)
		}
	}
	trace.haveTS, trace.lastTS, trace.frameTS = true, header.Timestamp, header.Timestamp
	trace.framePkt++
	if header.Marker {
		bucket.frames++
		if trace.framePkt > bucket.maxFramePackets {
			bucket.maxFramePackets = trace.framePkt
		}
	}
}

// observeRetransmit counts a resend; it carries an old sequence number, so it
// must not look like a gap.
func (trace *joinTrace) observeRetransmit() {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if !trace.done && trace.started {
		trace.current.rtx++
	}
}

func (trace *joinTrace) flushLocked() {
	if trace.emit != nil && (trace.current.media != 0 || trace.current.probe != 0 || trace.current.rtx != 0) {
		trace.emit(trace.current)
	}
}
