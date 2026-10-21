package nativeaudio

import (
	"encoding/binary"
	"errors"
)

// FLAC (RFC 9639) for the fixed 48 kHz stereo 16-bit frames this package
// carries. Every 20 ms frame decodes on its own: fixed linear prediction
// (orders 0-4) with partitioned Rice residuals, stereo decorrelation, and
// constant or verbatim subframes when those are smaller.

const (
	flacBitsPerSample  = 16
	flacMaxPartitions  = 6 // FrameSamples (960) divides by 2^6.
	flacMaxRiceParam   = 14
	flacStreamInfoSize = 34
)

// FLACStreamHeader is the "fLaC" marker and the only metadata block, STREAMINFO.
// WebCodecs takes it as the AudioDecoder description.
func FLACStreamHeader() []byte {
	var writer bitWriter
	writer.write('f', 8)
	writer.write('L', 8)
	writer.write('a', 8)
	writer.write('C', 8)
	writer.write(1, 1) // last metadata block
	writer.write(0, 7) // STREAMINFO
	writer.write(flacStreamInfoSize, 24)
	writer.write(FrameSamples, 16) // minimum block size
	writer.write(FrameSamples, 16) // maximum block size
	writer.write(0, 24)            // minimum frame size: unknown
	writer.write(0, 24)            // maximum frame size: unknown
	writer.write(SampleRate, 20)
	writer.write(Channels-1, 3)
	writer.write(flacBitsPerSample-1, 5)
	writer.write(0, 36) // total samples: a live stream has no end
	for range 4 {
		writer.write(0, 32) // MD5: not computed for a live stream
	}
	return writer.bytes
}

// FLACEncoder numbers frames; each Encode returns one complete FLAC frame.
type FLACEncoder struct {
	frame    uint32
	channels [4][FrameSamples]int32 // left, right, mid, side
}

func (encoder *FLACEncoder) Encode(pcm []byte) ([]byte, error) {
	if len(pcm) != FrameBytes {
		return nil, errors.New("FLAC input is not one PCM frame")
	}
	left, right := &encoder.channels[0], &encoder.channels[1]
	mid, side := &encoder.channels[2], &encoder.channels[3]
	for index := range FrameSamples {
		l := int32(int16(binary.LittleEndian.Uint16(pcm[index*4:])))
		r := int32(int16(binary.LittleEndian.Uint16(pcm[index*4+2:])))
		left[index], right[index] = l, r
		mid[index], side[index] = (l+r)>>1, l-r
	}
	// Side needs one more bit than its sources.
	costs := [4]int{
		bestSubframe(left[:], flacBitsPerSample).bits,
		bestSubframe(right[:], flacBitsPerSample).bits,
		bestSubframe(mid[:], flacBitsPerSample).bits,
		bestSubframe(side[:], flacBitsPerSample+1).bits,
	}
	assignment, first, second := uint64(0b0001), 0, 1 // independent
	best := costs[0] + costs[1]
	if cost := costs[0] + costs[3]; cost < best {
		assignment, first, second, best = 0b1000, 0, 3, cost // left/side
	}
	if cost := costs[3] + costs[1]; cost < best {
		assignment, first, second, best = 0b1001, 3, 1, cost // side/right
	}
	if cost := costs[2] + costs[3]; cost < best {
		assignment, first, second = 0b1010, 2, 3 // mid/side
	}

	var writer bitWriter
	writer.write(0b11111111111110, 14) // sync
	writer.write(0, 1)                 // reserved
	writer.write(0, 1)                 // fixed block size
	writer.write(0b0111, 4)            // block size: 16-bit value after the number
	writer.write(0b1010, 4)            // 48 kHz
	writer.write(assignment, 4)
	writer.write(0b100, 3) // 16 bits per sample
	writer.write(0, 1)     // reserved
	writeCodedNumber(&writer, uint64(encoder.frame))
	writer.write(FrameSamples-1, 16)
	writer.write(uint64(crc8(writer.bytes)), 8)
	for _, channel := range [2]int{first, second} {
		bits := uint(flacBitsPerSample)
		if channel == 3 {
			bits++
		}
		writeSubframe(&writer, encoder.channels[channel][:], bits)
	}
	writer.align()
	crc := crc16(writer.bytes)
	writer.write(uint64(crc), 16)
	encoder.frame = (encoder.frame + 1) & 0x7fffffff
	return writer.bytes, nil
}

type subframePlan struct {
	kind      int // 0 constant, 1 verbatim, 2 fixed
	order     int
	partition int
	params    [1 << flacMaxPartitions]uint8
	bits      int
}

// fixedResidual is RFC 9639 9.2.5's fixed predictor of the given order.
func fixedResidual(samples []int32, order, index int) int64 {
	x := func(back int) int64 { return int64(samples[index-back]) }
	switch order {
	case 0:
		return x(0)
	case 1:
		return x(0) - x(1)
	case 2:
		return x(0) - 2*x(1) + x(2)
	case 3:
		return x(0) - 3*x(1) + 3*x(2) - x(3)
	default:
		return x(0) - 4*x(1) + 6*x(2) - 4*x(3) + x(4)
	}
}

func zigzag(value int64) uint64 { return uint64((value << 1) ^ (value >> 63)) }

func riceBits(residuals []uint64, param uint) int {
	bits := 0
	for _, value := range residuals {
		bits += int(value>>param) + 1 + int(param)
	}
	return bits
}

// bestRiceParam evaluates the parameters around the mean's magnitude exactly.
func bestRiceParam(residuals []uint64) (uint8, int) {
	if len(residuals) == 0 {
		return 0, 0
	}
	var sum uint64
	for _, value := range residuals {
		sum += value
	}
	estimate := 0
	for mean := sum / uint64(len(residuals)); mean > 0 && estimate < flacMaxRiceParam; mean >>= 1 {
		estimate++
	}
	bestParam, bestBits := uint8(0), -1
	for param := max(0, estimate-2); param <= min(flacMaxRiceParam, estimate+1); param++ {
		if bits := riceBits(residuals, uint(param)); bestBits < 0 || bits < bestBits {
			bestParam, bestBits = uint8(param), bits
		}
	}
	return bestParam, bestBits
}

func bestSubframe(samples []int32, bps uint) subframePlan {
	constant := true
	for _, sample := range samples[1:] {
		if sample != samples[0] {
			constant = false
			break
		}
	}
	if constant {
		return subframePlan{kind: 0, bits: 8 + int(bps)}
	}
	plan := subframePlan{kind: 1, bits: 8 + len(samples)*int(bps)}
	// Choose the predictor by total residual magnitude, then price it exactly.
	bestOrder, bestMagnitude := 0, uint64(0)
	for order := range 5 {
		var magnitude uint64
		for index := order; index < len(samples); index++ {
			magnitude += zigzag(fixedResidual(samples, order, index))
		}
		if order == 0 || magnitude < bestMagnitude {
			bestOrder, bestMagnitude = order, magnitude
		}
	}
	residuals := make([]uint64, len(samples))
	for index := bestOrder; index < len(samples); index++ {
		residuals[index] = zigzag(fixedResidual(samples, bestOrder, index))
	}
	for partition := 0; partition <= flacMaxPartitions; partition++ {
		size := len(samples) >> partition
		if size <= bestOrder {
			break
		}
		candidate := subframePlan{kind: 2, order: bestOrder, partition: partition,
			bits: 8 + bestOrder*int(bps) + 2 + 4}
		for part := range 1 << partition {
			from := part * size
			if part == 0 {
				from = bestOrder
			}
			param, bits := bestRiceParam(residuals[from : (part+1)*size])
			candidate.params[part] = param
			candidate.bits += 4 + bits
		}
		if candidate.bits < plan.bits {
			plan = candidate
		}
	}
	return plan
}

func writeSubframe(writer *bitWriter, samples []int32, bps uint) {
	plan := bestSubframe(samples, bps)
	writer.write(0, 1) // padding
	switch plan.kind {
	case 0:
		writer.write(0b000000, 6)
		writer.write(0, 1) // no wasted bits
		writer.writeSigned(int64(samples[0]), bps)
	case 1:
		writer.write(0b000001, 6)
		writer.write(0, 1)
		for _, sample := range samples {
			writer.writeSigned(int64(sample), bps)
		}
	default:
		writer.write(uint64(0b001000|plan.order), 6)
		writer.write(0, 1)
		for index := range plan.order {
			writer.writeSigned(int64(samples[index]), bps)
		}
		writer.write(0b00, 2) // Rice coding with 4-bit parameters
		writer.write(uint64(plan.partition), 4)
		size := len(samples) >> plan.partition
		for part := range 1 << plan.partition {
			param := uint(plan.params[part])
			writer.write(uint64(param), 4)
			from := part * size
			if part == 0 {
				from = plan.order
			}
			for index := from; index < (part+1)*size; index++ {
				value := zigzag(fixedResidual(samples, plan.order, index))
				writer.writeUnary(value >> param)
				writer.write(value&(1<<param-1), param)
			}
		}
	}
}

// writeCodedNumber is FLAC's UTF-8-like frame number: a lead byte of B ones,
// a zero and the high bits, then B-1 continuation bytes of six bits each.
func writeCodedNumber(writer *bitWriter, value uint64) {
	if value < 1<<7 {
		writer.write(value, 8)
		return
	}
	length := 2
	for value >= 1<<(7-length+6*(length-1)) {
		length++
	}
	writer.write(uint64(0xff<<(8-length))&0xff|value>>(6*(length-1)), 8)
	for shift := 6 * (length - 2); shift >= 0; shift -= 6 {
		writer.write(0b10_000000|(value>>shift)&0x3f, 8)
	}
}

type bitWriter struct {
	bytes       []byte
	accumulator uint64
	pending     uint
}

// write appends the low bits of value, most significant first; bits <= 32.
func (writer *bitWriter) write(value uint64, bits uint) {
	if bits == 0 {
		return
	}
	writer.accumulator = writer.accumulator<<bits | value&(1<<bits-1)
	writer.pending += bits
	for writer.pending >= 8 {
		writer.pending -= 8
		writer.bytes = append(writer.bytes, byte(writer.accumulator>>writer.pending))
	}
	writer.accumulator &= 1<<writer.pending - 1
}

func (writer *bitWriter) writeSigned(value int64, bits uint) {
	writer.write(uint64(value), bits)
}

func (writer *bitWriter) writeUnary(zeros uint64) {
	for ; zeros >= 32; zeros -= 32 {
		writer.write(0, 32)
	}
	writer.write(1, uint(zeros)+1)
}

func (writer *bitWriter) align() {
	if writer.pending > 0 {
		writer.write(0, 8-writer.pending)
	}
}

func crc8(data []byte) byte {
	var crc byte
	for _, value := range data {
		crc ^= value
		for range 8 {
			if crc&0x80 != 0 {
				crc = crc<<1 ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func crc16(data []byte) uint16 {
	var crc uint16
	for _, value := range data {
		crc ^= uint16(value) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
