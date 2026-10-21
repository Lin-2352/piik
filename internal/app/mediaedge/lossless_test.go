package mediaedge

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/TNTcraftHIM/Piik/internal/app/nativeaudio"
	"github.com/pion/webrtc/v4"
)

func TestLosslessChannelDeliversFLACOnlyAfterTheViewerOptsIn(t *testing.T) {
	engine, err := NewEngine(EngineOptions{BindAddress: "127.0.0.1:0", IncludeLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	video, err := engine.NewSource("h264", 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	audio, err := engine.NewAudioSource(1, 128_000)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := engine.NewEdge(video, EdgeOptions{ConnectionID: "lossless-edge", Audio: audio})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = edge.Close(); _ = video.Close(); _ = audio.Close() })
	receiver := newAudioReceiver(t)
	t.Cleanup(func() { _ = receiver.Close() })
	opened := make(chan *webrtc.DataChannel, 1)
	messages := make(chan webrtc.DataChannelMessage, 64)
	receiver.OnDataChannel(func(channel *webrtc.DataChannel) {
		if channel.Label() != LosslessChannelLabel {
			return
		}
		channel.OnMessage(func(message webrtc.DataChannelMessage) { messages <- message })
		channel.OnOpen(func() { opened <- channel })
	})
	connectEdgeToReceiver(t, edge, receiver)
	var channel *webrtc.DataChannel
	select {
	case channel = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the lossless channel did not open")
	}

	pcm := func(seed byte) []byte {
		frame := make([]byte, nativeaudio.FrameBytes)
		for index := range frame {
			frame[index] = seed + byte(index*7)
		}
		return frame
	}
	// Before opting in, PCM still advances the timeline but nothing is sent.
	if err = audio.WritePCM(pcm(1), 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err = channel.SendText(`{"type":"start"}`); err != nil {
		t.Fatal(err)
	}
	next := func() webrtc.DataChannelMessage {
		t.Helper()
		select {
		case message := <-messages:
			return message
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a lossless message")
			return webrtc.DataChannelMessage{}
		}
	}
	header := next()
	var decoded losslessHeader
	if !header.IsString || json.Unmarshal(header.Data, &decoded) != nil || decoded.Type != "flac" ||
		decoded.SampleRate != 48_000 || decoded.Channels != 2 || decoded.FrameSamples != 960 {
		t.Fatalf("unexpected lossless header: %s", header.Data)
	}
	if description, err := base64.StdEncoding.DecodeString(decoded.Description); err != nil ||
		!bytes.Equal(description, nativeaudio.FLACStreamHeader()) {
		t.Fatalf("lossless header does not carry the FLAC stream header: %v", err)
	}

	var reference nativeaudio.FLACEncoder
	for frame := range 3 {
		input := pcm(byte(10 + frame))
		if err = audio.WritePCM(input, 20*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		message := next()
		want, err := reference.Encode(input)
		if err != nil {
			t.Fatal(err)
		}
		// The first frame before opt-in still consumed samples 0..959.
		if message.IsString || binary.BigEndian.Uint64(message.Data) != uint64((frame+1)*nativeaudio.FrameSamples) ||
			!bytes.Equal(message.Data[8:], want) {
			t.Fatalf("frame %d is not the FLAC encoding of its PCM at its sample index", frame)
		}
	}

	if err = channel.SendText(`{"type":"stop"}`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		audio.lossless.mu.Lock()
		subscribed := len(audio.lossless.channels)
		audio.lossless.mu.Unlock()
		if subscribed == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = audio.WritePCM(pcm(99), 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-messages:
		t.Fatalf("a stopped Viewer still received %d bytes", len(message.Data))
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRelayedAudioSourceOffersNoLosslessChannel(t *testing.T) {
	engine, err := NewEngine(EngineOptions{BindAddress: "127.0.0.1:0", IncludeLoopback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	video, err := engine.NewSource("h264", 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	audio, err := engine.NewRelayedAudioSource(1)
	if err != nil {
		t.Fatal(err)
	}
	edge, err := engine.NewEdge(video, EdgeOptions{ConnectionID: "relayed-audio-edge", Audio: audio})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = edge.Close(); _ = video.Close(); _ = audio.Close() })
	offer, err := edge.CreateOffer()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(offer.SDP), []byte("m=application")) {
		t.Fatal("relayed Opus has no PCM to offer losslessly")
	}
}
