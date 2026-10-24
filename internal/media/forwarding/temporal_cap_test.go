package forwarding

import (
	"testing"

	"github.com/livekit/livekit-server/pkg/sfu/buffer"
	"github.com/pion/rtp"
)

// capPacket is one RTP packet of an access unit in temporal layer tid.
func capPacket(esn uint64, tid int32, marker bool) *buffer.ExtPacket {
	return &buffer.ExtPacket{
		VideoLayer:        buffer.VideoLayer{Spatial: buffer.InvalidLayerSpatial, Temporal: tid},
		ExtSequenceNumber: esn,
		Packet:            &rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(esn), Marker: marker}},
	}
}

func TestTemporalCapPassesEveryPacketWhenUncapped(t *testing.T) {
	var cap temporalCap
	for esn, tid := range []int32{0, 1, 0, 1} {
		in := capPacket(uint64(100+esn), tid, true)
		out, ok := cap.admit(in, 1)
		if !ok || out != in {
			t.Fatalf("packet %d: admitted=%v same=%v, want the original packet", esn, ok, out == in)
		}
	}
}

func TestTemporalCapDropsEnhancementFramesWithoutSequenceHoles(t *testing.T) {
	var cap temporalCap
	cap.limit(0)
	// T0 (2 packets), T1 (2 packets), T0 (1 packet), T1 (1 packet), T0 (1 packet).
	frames := []struct {
		tid     int32
		packets int
	}{{0, 2}, {1, 2}, {0, 1}, {1, 1}, {0, 1}}
	esn := uint64(1000)
	var forwarded []uint64
	for _, frame := range frames {
		for i := 0; i < frame.packets; i++ {
			out, ok := cap.admit(capPacket(esn, frame.tid, i == frame.packets-1), 1)
			if ok != (frame.tid == 0) {
				t.Fatalf("esn %d tid %d: admitted=%v", esn, frame.tid, ok)
			}
			if ok {
				if out.Packet.SequenceNumber != uint16(out.ExtSequenceNumber) {
					t.Fatalf("header %d disagrees with extended %d", out.Packet.SequenceNumber, out.ExtSequenceNumber)
				}
				forwarded = append(forwarded, out.ExtSequenceNumber)
			}
			esn++
		}
	}
	want := []uint64{1000, 1001, 1002, 1003}
	if len(forwarded) != len(want) {
		t.Fatalf("forwarded %v, want %v", forwarded, want)
	}
	for i := range want {
		if forwarded[i] != want[i] {
			t.Fatalf("forwarded %v, want %v", forwarded, want)
		}
	}
}

func TestTemporalCapMapsRenumberedPacketsBackForResends(t *testing.T) {
	var cap temporalCap
	cap.limit(0)
	cap.admit(capPacket(500, 0, true), 1)
	cap.admit(capPacket(501, 1, true), 1)
	cap.admit(capPacket(502, 1, true), 1)
	out, _ := cap.admit(capPacket(503, 0, true), 1)
	if out.ExtSequenceNumber != 501 {
		t.Fatalf("renumbered to %d, want 501", out.ExtSequenceNumber)
	}
	if got := cap.original(1, 501); got != 503 {
		t.Fatalf("resend of 501 reads %d, want the source packet 503", got)
	}
	if got := cap.original(1, 500); got != 500 {
		t.Fatalf("resend of 500 reads %d, want 500", got)
	}
}

func TestTemporalCapChangesOnlyAtFrameBoundaries(t *testing.T) {
	var cap temporalCap
	// A T1 frame is mid-way when the cap arrives: it must finish intact.
	if _, ok := cap.admit(capPacket(10, 1, false), 1); !ok {
		t.Fatal("uncapped first packet dropped")
	}
	cap.limit(0)
	if _, ok := cap.admit(capPacket(11, 1, true), 1); !ok {
		t.Fatal("the rest of a started frame must not be cut")
	}
	if _, ok := cap.admit(capPacket(12, 1, true), 1); ok {
		t.Fatal("the next enhancement frame must be dropped")
	}
	// Lifting the cap mid-frame likewise waits for the next frame.
	cap.admit(capPacket(13, 0, false), 1)
	cap.limit(-1)
	if _, ok := cap.admit(capPacket(14, 0, true), 1); !ok {
		t.Fatal("base frame dropped")
	}
	if out, ok := cap.admit(capPacket(15, 1, true), 1); !ok || out.ExtSequenceNumber != 14 {
		t.Fatalf("after lifting: admitted=%v esn=%v, want 14 (offset kept)", ok, out)
	}
}

func TestTemporalCapNumbersEachLayerIndependently(t *testing.T) {
	var cap temporalCap
	cap.limit(0)
	cap.admit(capPacket(1, 1, true), 1) // dropped on layer 1 only
	if out, _ := cap.admit(capPacket(70, 0, true), 0); out.ExtSequenceNumber != 70 {
		t.Fatalf("layer 0 renumbered to %d, want 70", out.ExtSequenceNumber)
	}
	if out, _ := cap.admit(capPacket(2, 0, true), 1); out.ExtSequenceNumber != 1 {
		t.Fatalf("layer 1 renumbered to %d, want 1", out.ExtSequenceNumber)
	}
}

// A capped viewer sees ~4,000 packets/s at 120 fps; the cap sits on that path.
func BenchmarkTemporalCapAdmit(b *testing.B) {
	for _, capped := range []bool{false, true} {
		name := "uncapped"
		if capped {
			name = "capped"
		}
		b.Run(name, func(b *testing.B) {
			var cap temporalCap
			if capped {
				cap.limit(0)
			}
			packets := [4]*buffer.ExtPacket{}
			for i := range packets {
				packets[i] = capPacket(0, int32(i%2), i%2 == 1)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				packet := packets[i%4]
				packet.ExtSequenceNumber = uint64(i)
				cap.admit(packet, 1)
			}
		})
	}
}

func BenchmarkTemporalCapOriginal(b *testing.B) {
	var cap temporalCap
	cap.limit(0)
	for i := uint64(0); i < 20000; i++ {
		cap.admit(capPacket(i, int32(i%2), true), 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		cap.original(1, uint64(i%10000))
	}
}

// The forwarder can lag the encoded source (a large key frame fanned out to many
// viewers); a frame's layer must stay known for seconds, not a couple of frames.
func TestFrameTemporalSurvivesAForwarderBacklog(t *testing.T) {
	var source Source
	const frames = 900 // 7.5 s at 120 fps
	for frame := 0; frame < frames; frame++ {
		source.recordFrameTemporal(0, uint32(frame)*750, int32(frame%2))
	}
	for _, frame := range []int{0, 100, 450, 899} {
		got, known := source.FrameTemporal(0, uint32(frame)*750)
		if !known || got != int32(frame%2) {
			t.Fatalf("frame %d: layer %d known=%v, want %d known", frame, got, known, frame%2)
		}
	}
}
