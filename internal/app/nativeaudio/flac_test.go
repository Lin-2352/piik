package nativeaudio

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

func TestFLACChecksumsUseTheSpecifiedPolynomials(t *testing.T) {
	// Standard check values for "123456789": CRC-8 (0x07) and CRC-16/UMTS (0x8005).
	if got := crc8([]byte("123456789")); got != 0xf4 {
		t.Fatalf("crc8 = %#x", got)
	}
	if got := crc16([]byte("123456789")); got != 0xfee8 {
		t.Fatalf("crc16 = %#x", got)
	}
}

func TestFLACFrameNumbersUseTheUTF8Layout(t *testing.T) {
	for _, value := range []uint64{0, 0x7f, 0x80, 0x7ff, 0x800, 0xffff, 0x10000, 0x10ffff} {
		var writer bitWriter
		writeCodedNumber(&writer, value)
		if want := utf8.AppendRune(nil, rune(value)); !bytes.Equal(writer.bytes, want) {
			t.Fatalf("coded %#x = %x, want %x", value, writer.bytes, want)
		}
	}
	// Beyond Unicode, FLAC continues the pattern up to 31 bits in six bytes.
	var writer bitWriter
	writeCodedNumber(&writer, 0x7fffffff)
	if !bytes.Equal(writer.bytes, []byte{0xfd, 0xbf, 0xbf, 0xbf, 0xbf, 0xbf}) {
		t.Fatalf("coded 2^31-1 = %x", writer.bytes)
	}
}

// pcmFrames covers the subframe kinds and stereo assignments the encoder picks.
func pcmFrames() [][]byte {
	random := rand.New(rand.NewPCG(1, 2))
	var frames [][]byte
	sample := 0
	for frame := range 240 {
		pcm := make([]byte, FrameBytes)
		for index := range FrameSamples {
			var left, right float64
			phase := float64(sample) / SampleRate
			switch frame / 40 {
			case 0: // silence: constant subframes
			case 1: // correlated tone: mid/side
				left = 12_000 * math.Sin(2*math.Pi*440*phase)
				right = left * 0.9
			case 2: // independent tones
				left = 9_000 * math.Sin(2*math.Pi*220*phase)
				right = 7_000 * math.Sin(2*math.Pi*1_337*phase)
			case 3: // white noise: verbatim beats prediction
				left = float64(random.IntN(65_536) - 32_768)
				right = float64(random.IntN(65_536) - 32_768)
			case 4: // full-scale square in anti-phase: extreme side values
				left = 32_767
				if (sample/37)%2 == 1 {
					left = -32_768
				}
				right = -left - 1
			default: // music-like mix with a DC offset
				left = 5_000*math.Sin(2*math.Pi*110*phase) + 3_000*math.Sin(2*math.Pi*2_200*phase) + 400
				right = 5_000*math.Sin(2*math.Pi*111*phase) + float64(random.IntN(200)-100)
			}
			binary.LittleEndian.PutUint16(pcm[index*4:], uint16(int16(max(-32_768, min(32_767, left)))))
			binary.LittleEndian.PutUint16(pcm[index*4+2:], uint16(int16(max(-32_768, min(32_767, right)))))
			sample++
		}
		frames = append(frames, pcm)
	}
	return frames
}

func TestFLACStreamDecodesBitExactlyWithFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed; it is the independent reference decoder")
	}
	frames := pcmFrames()
	var encoder FLACEncoder
	stream := FLACStreamHeader()
	var original []byte
	var encodedBytes int
	for _, pcm := range frames {
		frame, err := encoder.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		encodedBytes += len(frame)
		stream = append(stream, frame...)
		original = append(original, pcm...)
	}
	directory := t.TempDir()
	input, output := filepath.Join(directory, "in.flac"), filepath.Join(directory, "out.raw")
	if err := os.WriteFile(input, stream, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(ffmpeg, "-v", "error", "-xerror", "-i", input, "-f", "s16le", "-acodec", "pcm_s16le", output)
	if report, err := command.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg rejected the stream: %v\n%s", err, report)
	}
	decoded, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, original) {
		t.Fatalf("decoded %d bytes differ from %d original bytes", len(decoded), len(original))
	}
	t.Logf("FLAC %d bytes for %d PCM bytes (%.1f%%)", encodedBytes, len(original),
		100*float64(encodedBytes)/float64(len(original)))
}

// The Viewer's capability probe (src/client/media/lossless-audio.ts) carries
// these exact bytes; tests/lossless-audio.test.ts pins the same value.
func TestFLACStreamHeaderIsTheSharedDescription(t *testing.T) {
	const want = "664c61438000002203c003c00000000000000bb802f00000000000000000000000000000000000000000"
	if got := hex.EncodeToString(FLACStreamHeader()); got != want {
		t.Fatalf("FLAC stream header = %s", got)
	}
}

func TestFLACRejectsAPartialFrame(t *testing.T) {
	var encoder FLACEncoder
	if _, err := encoder.Encode(make([]byte, FrameBytes-4)); err == nil {
		t.Fatal("a partial PCM frame was encoded")
	}
}
