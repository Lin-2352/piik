package mediaedge

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"sync"

	"github.com/TNTcraftHIM/Piik/internal/app/nativeaudio"
	"github.com/pion/webrtc/v4"
)

// LosslessChannelLabel names the optional per-edge channel that carries the
// same mixed PCM as the Opus track, FLAC-encoded. Opus stays the synchronized
// default; a Viewer opts in by sending {"type":"start"} and can stop any time.
const LosslessChannelLabel = "piik-lossless-audio"

// A Viewer that cannot keep up falls back to Opus instead of building latency:
// about 0.65 s of FLAC at the PCM worst case.
const losslessMaxBufferedBytes = 128 * 1024

type losslessHeader struct {
	Type          string `json:"type"`
	SampleRate    int    `json:"sampleRate"`
	Channels      int    `json:"channels"`
	BitsPerSample int    `json:"bitsPerSample"`
	FrameSamples  int    `json:"frameSamples"`
	// Description is the FLAC stream header (fLaC + STREAMINFO), base64.
	Description string `json:"description"`
}

type losslessStopped struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type losslessOutput struct {
	mu       sync.Mutex
	encoder  nativeaudio.FLACEncoder
	channels map[*Edge]*webrtc.DataChannel
	// Samples counts every PCM frame, encoded or not, so a Viewer that starts
	// later still receives the source's sample timeline.
	samples uint64
}

// openLosslessChannel negotiates the channel in the edge's offer. Only a PCM
// source can produce lossless audio; relayed Opus never had the samples.
func (source *AudioSource) openLosslessChannel(edge *Edge) error {
	if source == nil || source.encoder == nil {
		return nil
	}
	ordered := true
	channel, err := edge.connection.CreateDataChannel(LosslessChannelLabel, &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		return err
	}
	channel.OnMessage(func(message webrtc.DataChannelMessage) {
		var request struct {
			Type string `json:"type"`
		}
		if !message.IsString || len(message.Data) > 256 || json.Unmarshal(message.Data, &request) != nil {
			return
		}
		switch request.Type {
		case "start":
			source.startLossless(edge, channel)
		case "stop":
			source.stopLossless(edge, "")
		}
	})
	channel.OnClose(func() { source.stopLossless(edge, "") })
	return nil
}

func (source *AudioSource) startLossless(edge *Edge, channel *webrtc.DataChannel) {
	header, err := json.Marshal(losslessHeader{
		Type: "flac", SampleRate: nativeaudio.SampleRate, Channels: nativeaudio.Channels,
		BitsPerSample: 16, FrameSamples: nativeaudio.FrameSamples,
		Description: base64.StdEncoding.EncodeToString(nativeaudio.FLACStreamHeader()),
	})
	if err != nil {
		return
	}
	source.lossless.mu.Lock()
	defer source.lossless.mu.Unlock()
	source.mu.Lock()
	_, attached := source.edges[edge]
	source.mu.Unlock()
	if !attached {
		return
	}
	// The header precedes every frame: both go through this lock in order.
	if channel.SendText(string(header)) != nil {
		return
	}
	if source.lossless.channels == nil {
		source.lossless.channels = make(map[*Edge]*webrtc.DataChannel)
	}
	source.lossless.channels[edge] = channel
}

func (source *AudioSource) stopLossless(edge *Edge, reason string) {
	source.lossless.mu.Lock()
	channel := source.lossless.channels[edge]
	delete(source.lossless.channels, edge)
	source.lossless.mu.Unlock()
	if channel != nil && reason != "" {
		if notice, err := json.Marshal(losslessStopped{Type: "stopped", Reason: reason}); err == nil {
			_ = channel.SendText(string(notice))
		}
	}
}

// writeLossless encodes one PCM frame once for every subscribed edge. Each
// message is the frame's first sample index (big-endian uint64) and the frame.
func (source *AudioSource) writeLossless(pcm []byte) {
	output := &source.lossless
	output.mu.Lock()
	first := output.samples
	output.samples += nativeaudio.FrameSamples
	if len(output.channels) == 0 {
		output.mu.Unlock()
		return
	}
	frame, err := output.encoder.Encode(pcm)
	if err != nil {
		output.mu.Unlock()
		return
	}
	message := make([]byte, 8+len(frame))
	binary.BigEndian.PutUint64(message, first)
	copy(message[8:], frame)
	var congested []*Edge
	for edge, channel := range output.channels {
		if channel.BufferedAmount() > losslessMaxBufferedBytes {
			congested = append(congested, edge)
			continue
		}
		_ = channel.Send(message)
	}
	output.mu.Unlock()
	for _, edge := range congested {
		source.stopLossless(edge, "congested")
	}
}
