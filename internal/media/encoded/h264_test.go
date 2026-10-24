package encoded

import (
	"bytes"
	"testing"
)

// The access units NVIDIA's H.264 encoder emits with two temporal layers.
var (
	h264BaseKey = []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x34, 0, 0, 0, 1, 0x68, 0xce,
		0, 0, 0, 1, 0x6e, 0xc0, 0x80, 0x07, 0, 0, 0, 1, 0x65, 0x88}
	h264Enhancement = []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x0e, 0x40, 0x80, 0x27, 0, 0, 0, 1, 0x01, 0x9a}
)

func TestH264TemporalLayerReadsAndStripsTheSVCPrefix(t *testing.T) {
	for _, check := range []struct {
		name     string
		au, want []byte
		temporal int32
	}{
		{"base key frame", h264BaseKey, []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x34,
			0, 0, 0, 1, 0x68, 0xce, 0, 0, 0, 1, 0x65, 0x88}, 0},
		{"enhancement", h264Enhancement, []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x01, 0x9a}, 1},
	} {
		got, temporal := H264TemporalLayer(check.au)
		if temporal != check.temporal || !bytes.Equal(got, check.want) {
			t.Fatalf("%s: temporal %d, data % x; want %d, % x", check.name, temporal, got, check.temporal, check.want)
		}
	}
}

func TestH264TemporalLayerLeavesASingleLayerStreamUntouched(t *testing.T) {
	au := []byte{0, 0, 0, 1, 0x41, 0x9a, 0x10, 0, 0, 1, 0x41, 0x9b}
	got, temporal := H264TemporalLayer(au)
	if temporal != 0 || &got[0] != &au[0] || len(got) != len(au) {
		t.Fatalf("single-layer access unit was rewritten: temporal %d, % x", temporal, got)
	}
}

// Encoders without SVC prefix NAL units still mark enhancement frames as
// non-reference slices; nothing references such a frame, so it can be dropped.
func TestH264TemporalLayerTreatsNonReferenceFramesAsDroppable(t *testing.T) {
	for _, check := range []struct {
		name     string
		au       []byte
		temporal int32
	}{
		{"non-reference slice, no prefix", []byte{0, 0, 0, 1, 0x01, 0x9a}, 1},
		{"non-reference slice behind an AUD", []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x01, 0x9a}, 1},
		{"reference slice", []byte{0, 0, 0, 1, 0x41, 0x9a}, 0},
		{"key frame stays in the base layer", h264BaseKey, 0},
		{"a frame with any reference slice stays", []byte{0, 0, 0, 1, 0x01, 0x9a, 0, 0, 0, 1, 0x41, 0x9a}, 0},
		{"parameter sets alone are not a droppable frame", []byte{0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x34, 0, 0, 0, 1, 0x68, 0xce}, 0},
		{"SEI nal_ref_idc is always 0 and says nothing", []byte{0, 0, 0, 1, 0x06, 0x05, 0, 0, 0, 1, 0x41, 0x9a}, 0},
	} {
		if _, temporal := H264TemporalLayer(check.au); temporal != check.temporal {
			t.Fatalf("%s: temporal %d, want %d", check.name, temporal, check.temporal)
		}
	}
}

// An encoder that marks its enhancement layer as reference frames (a P frame
// the next frame predicts from) must never lose it, whatever the prefix says:
// dropping it would break every later frame of a capped viewer.
func TestH264TemporalLayerNeverDropsAReferencedFrameTheEncoderCallsEnhancement(t *testing.T) {
	au := []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x0e, 0x40, 0x80, 0x27, 0, 0, 0, 1, 0x41, 0x9a}
	got, temporal := H264TemporalLayer(au)
	if temporal != 0 {
		t.Fatalf("a frame with a reference slice reported temporal layer %d, want 0", temporal)
	}
	if want := []byte{0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x41, 0x9a}; !bytes.Equal(got, want) {
		t.Fatalf("prefix NAL not stripped: % x", got)
	}
}
