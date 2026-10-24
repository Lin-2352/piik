package forwarding

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TNTcraftHIM/Piik/internal/media/encoded"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
)

// Browser VP8 can produce L1T3 even with only one spatial encoding. Both
// forwarding owners must retain its enhancement frames, not silently cap T0.
// This exercises RTP forwarding; the Browser gate separately checks decoding.
func TestForwardingPreservesVP8TemporalLayers(t *testing.T) {
	for _, name := range []string{"transport", "publication"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			codec := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeVP8, ClockRate: 90_000,
			}, PayloadType: 96}
			source, err := NewSource(SourceOptions{ID: "screen", StreamID: "temporal", Codec: codec,
				Formats: []LayerFormat{{Width: 8, Height: 8, Bitrate: 300_000}}})
			check(err)
			defer source.Close()
			check(source.BindLayer(0, 1000, webrtc.RTPParameters{Codecs: []webrtc.RTPCodecParameters{codec}}))
			settings := webrtc.SettingEngine{}
			settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
			settings.SetIncludeLoopbackCandidate(true)
			settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
			options := TransportOptions{Source: source, Settings: settings, ConnectionID: name, InitialBitrate: 1_000_000}
			var pc *webrtc.PeerConnection
			var connect func() error
			if name == "publication" {
				publication, err := NewPublication(options)
				check(err)
				defer publication.Close()
				pc, connect = publication.PC, publication.SetConnected
			} else {
				transport, err := NewTransport(options)
				check(err)
				defer transport.Close()
				pc, connect = transport.PC, transport.SetConnected
			}
			receiver, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
			check(err)
			defer receiver.Close()
			var received [3]atomic.Uint32
			receiver.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
				for {
					packet, _, err := track.ReadRTP()
					if err != nil {
						return
					}
					var vp8 codecs.VP8Packet
					if _, err := vp8.Unmarshal(packet.Payload); err == nil && vp8.T == 1 && vp8.TID < 3 {
						received[vp8.TID].Add(1)
					}
				}
			})
			offer, err := pc.CreateOffer(nil)
			check(err)
			gather := webrtc.GatheringCompletePromise(pc)
			check(pc.SetLocalDescription(offer))
			select {
			case <-gather:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			check(receiver.SetRemoteDescription(*pc.LocalDescription()))
			answer, err := receiver.CreateAnswer(nil)
			check(err)
			gather = webrtc.GatheringCompletePromise(receiver)
			check(receiver.SetLocalDescription(answer))
			select {
			case <-gather:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			check(pc.SetRemoteDescription(*receiver.LocalDescription()))
			for pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
				select {
				case <-time.After(5 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			check(connect())
			for frame := uint16(0); received[0].Load() < 4 || received[1].Load() < 4 || received[2].Load() < 4; frame++ {
				tid := []byte{0, 2, 1, 2}[frame%4]
				// RFC 7741: I/L/T descriptor, PictureID, TL0PICIDX, TID/Y.
				payload := append([]byte{0x90, 0xe0, byte(frame & 0x7f), byte(frame / 4), tid<<6 | 0x20}, sfu.VP8KeyFrame8x8...)
				if frame%20 != 0 {
					payload[5] |= 1 // Delta-frame header; only T0 carries keyframes.
				}
				check(source.WriteRTP(0, &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true,
					PayloadType: 96, SSRC: 1000, SequenceNumber: frame + 1, Timestamp: uint32(frame+1) * 3000}, Payload: payload}))
				select {
				case <-time.After(time.Second / 30):
				case <-ctx.Done():
					t.Fatalf("forwarding discarded temporal layers: T0=%d T1=%d T2=%d", received[0].Load(), received[1].Load(), received[2].Load())
				}
			}
		})
	}
}

// A viewer that cannot decode the full frame rate gets only H.265 base-layer
// frames, with a sequence the receiver sees as gap-free (no false loss/NACKs).
func TestCappedTransportForwardsOnlyH265BaseLayerWithoutGaps(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	codec := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH265, ClockRate: 90_000,
	}, PayloadType: 96}
	source, err := NewSource(SourceOptions{ID: "screen", StreamID: "capped", Codec: codec,
		Formats: []LayerFormat{{Width: 8, Height: 8, Bitrate: 300_000}}})
	check(err)
	defer source.Close()
	check(source.BindLayer(0, 1000, webrtc.RTPParameters{Codecs: []webrtc.RTPCodecParameters{codec}}))
	settings := webrtc.SettingEngine{}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	transport, err := NewTransport(TransportOptions{Source: source, Settings: settings, ConnectionID: "phone", InitialBitrate: 1_000_000})
	check(err)
	defer transport.Close()
	transport.Output.CapTemporalLayer(0)
	receiver, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	check(err)
	defer receiver.Close()
	var base, enhancement, gaps atomic.Uint32
	receiver.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		last := -1
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if len(packet.Payload) < 2 {
				continue
			}
			if last >= 0 && packet.SequenceNumber != uint16(last+1) {
				gaps.Add(1)
			}
			last = int(packet.SequenceNumber)
			if packet.Payload[1]&0x07 == 1 {
				base.Add(1)
			} else {
				enhancement.Add(1)
			}
		}
	})
	offer, err := transport.PC.CreateOffer(nil)
	check(err)
	gather := webrtc.GatheringCompletePromise(transport.PC)
	check(transport.PC.SetLocalDescription(offer))
	<-gather
	check(receiver.SetRemoteDescription(*transport.PC.LocalDescription()))
	answer, err := receiver.CreateAnswer(nil)
	check(err)
	gather = webrtc.GatheringCompletePromise(receiver)
	check(receiver.SetLocalDescription(answer))
	<-gather
	check(transport.PC.SetRemoteDescription(*receiver.LocalDescription()))
	for transport.PC.ConnectionState() != webrtc.PeerConnectionStateConnected {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	check(transport.SetConnected())
	for frame := uint16(0); base.Load() < 12; frame++ {
		// HEVC NAL header: type<<1, then nuh_temporal_id_plus1. Every eighth
		// frame is an SPS (what LiveKit treats as an H.265 keyframe), so a late
		// forwarder start can still lock on; odd frames are the enhancement
		// temporal layer (TID 1).
		header := []byte{1 << 1, 1 + byte(frame%2)}
		if frame%8 == 0 {
			header = []byte{33 << 1, 1}
		}
		check(source.WriteRTP(0, &rtp.Packet{Header: rtp.Header{Version: 2, Marker: true, PayloadType: 96,
			SSRC: 1000, SequenceNumber: frame + 1, Timestamp: uint32(frame+1) * 750}, Payload: append(header, 0xaf, 0x01)}))
		select {
		case <-time.After(time.Second / 120):
		case <-ctx.Done():
			t.Fatalf("capped viewer stalled: base=%d enhancement=%d", base.Load(), enhancement.Load())
		}
	}
	if enhancement.Load() != 0 || gaps.Load() != 0 {
		t.Fatalf("capped viewer got %d enhancement packets and %d sequence gaps", enhancement.Load(), gaps.Load())
	}
}

// The same cap for native H.264: LiveKit reads no temporal layer from H.264, so
// the encoded source classifies each access unit from its SVC prefix NAL, strips
// the prefix, and the capped viewer receives only base frames without gaps.
func TestCappedTransportForwardsOnlyH264BaseLayerWithoutGaps(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	codec := webrtc.RTPCodecParameters{RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeH264, ClockRate: 90_000,
		SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
	}, PayloadType: 96}
	input, err := NewEncodedSource(SourceOptions{ID: "screen", StreamID: "capped-h264", Codec: codec,
		Formats: []LayerFormat{{Width: 2, Height: 2, Bitrate: 300_000}}})
	check(err)
	defer input.Close()
	settings := webrtc.SettingEngine{}
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	transport, err := NewTransport(TransportOptions{Source: input.Source, Settings: settings, ConnectionID: "phone", InitialBitrate: 1_000_000})
	check(err)
	defer transport.Close()
	transport.Output.CapTemporalLayer(0)
	receiver, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	check(err)
	defer receiver.Close()
	var base, enhancement, prefixes, gaps atomic.Uint32
	receiver.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		last := -1
		for {
			packet, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if len(packet.Payload) == 0 {
				continue
			}
			if last >= 0 && packet.SequenceNumber != uint16(last+1) {
				gaps.Add(1)
			}
			last = int(packet.SequenceNumber)
			switch nal := packet.Payload[0]; {
			case nal&0x1f == 14:
				prefixes.Add(1)
			case nal&0x1f == 1 && nal&0x60 == 0:
				enhancement.Add(1)
			case nal&0x1f == 1 || nal&0x1f == 5:
				base.Add(1)
			}
		}
	})
	offer, err := transport.PC.CreateOffer(nil)
	check(err)
	gather := webrtc.GatheringCompletePromise(transport.PC)
	check(transport.PC.SetLocalDescription(offer))
	<-gather
	check(receiver.SetRemoteDescription(*transport.PC.LocalDescription()))
	answer, err := receiver.CreateAnswer(nil)
	check(err)
	gather = webrtc.GatheringCompletePromise(receiver)
	check(receiver.SetLocalDescription(answer))
	<-gather
	check(transport.PC.SetRemoteDescription(*receiver.LocalDescription()))
	for transport.PC.ConnectionState() != webrtc.PeerConnectionStateConnected {
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	check(transport.SetConnected())
	nal := func(units ...[]byte) []byte {
		var au []byte
		for _, unit := range units {
			au = append(append(au, 0, 0, 0, 1), unit...)
		}
		return au
	}
	prefix := func(tid byte) []byte { return []byte{0x6e, 0xc0, 0x80, tid<<5 | 0x07} }
	period := time.Second / 120
	for frame := 0; base.Load() < 12; frame++ {
		// Every eighth frame is a key frame; odd frames are the non-reference
		// enhancement layer (prefix temporal_id 1, nal_ref_idc 0).
		au := nal(prefix(0), []byte{0x41, 0x9a, 0x02})
		switch {
		case frame%8 == 0:
			au = nal(sfu.H264KeyFrame2x2SPS, sfu.H264KeyFrame2x2PPS, prefix(0), sfu.H264KeyFrame2x2IDR)
		case frame%2 == 1:
			au = nal(append([]byte{0x0e}, prefix(1)[1:]...), []byte{0x01, 0x9a, 0x02})
		}
		pts := time.Duration(frame) * period
		check(input.BeginFrame(pts, time.Now()))
		check(input.WriteFrame(0, encoded.Frame{Data: au, PTS: pts, Duration: period}))
		select {
		case <-time.After(period):
		case <-ctx.Done():
			t.Fatalf("capped H.264 viewer stalled: base=%d enhancement=%d", base.Load(), enhancement.Load())
		}
	}
	if enhancement.Load() != 0 || prefixes.Load() != 0 || gaps.Load() != 0 {
		t.Fatalf("capped H.264 viewer got %d enhancement packets, %d prefix NALs and %d sequence gaps",
			enhancement.Load(), prefixes.Load(), gaps.Load())
	}
}
