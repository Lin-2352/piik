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
