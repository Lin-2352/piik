package encoded

import "encoding/hex"

// H264ConstrainedBaselineLevel recognizes the equivalent coding-tool subsets
// in RFC 6184 Table 5. Callers own their permitted level range.
func H264ConstrainedBaselineLevel(value string) (byte, bool) {
	if len(value) != 6 {
		return 0, false
	}
	profile, err := hex.DecodeString(value)
	if err != nil {
		return 0, false
	}
	constrainedBaseline := (profile[0] == 0x42 && profile[1]&0x4f == 0x40) ||
		(profile[0] == 0x4d && profile[1]&0x8f == 0x80) ||
		(profile[0] == 0x58 && profile[1]&0xcf == 0xc0)
	return profile[2], constrainedBaseline
}

const h264PrefixNAL = 14

// H264TemporalLayer returns an Annex B access unit without its SVC prefix NAL
// units and the temporal layer it belongs to: 1 for a frame nothing references,
// otherwise 0. With temporal layers an encoder precedes each slice with a prefix
// NAL declaring its layer; a base-layer AVC decoder ignores it, so it is removed
// rather than relying on every browser decoder to skip it. An encoder that adds
// layers without prefix NAL units still marks the enhancement frames as
// non-reference slices (nal_ref_idc 0), and such a frame can be dropped whatever
// its prefix says. A key frame, or any frame with a reference slice, is layer 0.
// An access unit without prefix NAL units is returned unchanged.
func H264TemporalLayer(au []byte) ([]byte, int32) {
	temporal := int32(0)
	slices, referenced := 0, false
	var out []byte
	for start := 0; start < len(au); {
		begin, payload := annexBStartCode(au, start)
		if begin < 0 {
			break
		}
		end, _ := annexBStartCode(au, payload)
		if end < 0 {
			end = len(au)
		}
		if payload < end {
			switch nalType := au[payload] & 0x1f; nalType {
			case h264PrefixNAL:
				if end-payload >= 4 {
					temporal = max(temporal, int32(au[payload+3]>>5))
				}
				if out == nil {
					out = append(make([]byte, 0, len(au)), au[:begin]...)
				}
				start = end
				continue
			case 1, 5: // coded slices
				slices++
				referenced = referenced || au[payload]&0x60 != 0 || nalType == 5
			}
		}
		if out != nil {
			out = append(out, au[begin:end]...)
		}
		start = end
	}
	if referenced {
		temporal = 0 // later frames predict from it: dropping it breaks them
	} else if slices > 0 {
		temporal = max(temporal, 1)
	}
	if out == nil {
		return au, temporal
	}
	return out, temporal
}

// annexBStartCode finds the next 3- or 4-byte start code at or after from and
// returns where it begins and where its NAL unit starts, or -1.
func annexBStartCode(data []byte, from int) (int, int) {
	for i := from; i+3 <= len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 {
			if data[i+2] == 1 {
				if i > from && data[i-1] == 0 {
					return i - 1, i + 3
				}
				return i, i + 3
			}
		}
	}
	return -1, -1
}
