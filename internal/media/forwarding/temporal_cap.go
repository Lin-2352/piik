package forwarding

import (
	"sync"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/pion/rtp"
)

// LiveKit forwards H.264/H.265 through its simulcast selector, which never
// filters temporal layers (only its VP8 munger does). temporalCap drops the
// enhancement temporal layer for one viewer and renumbers the remaining
// packets, so that viewer sees a gap-free sequence and its NACKs still resolve
// to the source packets. Base-layer frames reference only base-layer frames,
// so the cap can change at any frame boundary without a keyframe.
type temporalCap struct {
	mu      sync.Mutex // admit runs on the write path, original on NACK handling
	pending int32      // highest temporal layer to forward; -1 forwards everything
	layers  map[int32]*capLayer
	set     bool
}

// Renumbered packets must still map back for as long as the source buffer can
// serve a resend, so the window matches the retransmission cache.
const capWindow = 1 << 12

type capLayer struct {
	max       int32
	dropped   uint64
	midFrame  bool
	originals [capWindow]uint64
}

func (cap *temporalCap) limit(max int32) {
	cap.mu.Lock()
	defer cap.mu.Unlock()
	cap.pending, cap.set = max, true
}

// reset forgets the renumbering when the viewer moves to another source, whose
// sequence space is unrelated. The limit itself stays.
func (cap *temporalCap) reset() {
	cap.mu.Lock()
	defer cap.mu.Unlock()
	cap.layers = nil
}

func (cap *temporalCap) admit(packet *buffer.ExtPacket, layer int32) (*buffer.ExtPacket, bool) {
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.layers == nil {
		cap.layers = make(map[int32]*capLayer)
	}
	state := cap.layers[layer]
	if state == nil {
		state = &capLayer{max: -1}
		cap.layers[layer] = state
	}
	if !state.midFrame && cap.set {
		state.max = cap.pending
	}
	state.midFrame = !packet.Packet.Marker
	if state.max >= 0 && packet.Temporal > state.max {
		state.dropped++
		return packet, false
	}
	esn := packet.ExtSequenceNumber - state.dropped
	state.originals[esn%capWindow] = packet.ExtSequenceNumber
	if state.dropped == 0 {
		return packet, true
	}
	renumbered := *packet
	header := *packet.Packet
	header.Header.SequenceNumber = uint16(esn)
	renumbered.Packet = &rtp.Packet{Header: header.Header, Payload: header.Payload, PaddingSize: header.PaddingSize}
	renumbered.ExtSequenceNumber = esn
	return &renumbered, true
}

// original maps a renumbered sequence number back to the source packet.
func (cap *temporalCap) original(layer int32, esn uint64) uint64 {
	cap.mu.Lock()
	defer cap.mu.Unlock()
	state := cap.layers[layer]
	if state == nil || state.dropped == 0 {
		return esn
	}
	if source := state.originals[esn%capWindow]; source >= esn && source-esn <= state.dropped {
		return source
	}
	return esn
}

// CapTemporalLayer limits this viewer to temporal layers <= max; -1 lifts it.
// It takes effect at the next frame boundary of each layer.
func (output *Output) CapTemporalLayer(max int32) {
	output.cap.limit(max)
}

// ReadRTP serves this viewer's NACKs: renumbered packets map back to the source.
func (receiver *outputReceiver) ReadRTP(buf []byte, layer uint8, esn uint64) (int, error) {
	if receiver.output != nil {
		esn = receiver.output.cap.original(int32(layer), esn)
	}
	return receiver.Source.ReadRTP(buf, layer, esn)
}
