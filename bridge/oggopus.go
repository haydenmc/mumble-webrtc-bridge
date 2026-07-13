package bridge

import (
	"encoding/binary"
	"io"
)

// oggOpusWriter is a minimal Ogg container muxer for Opus audio (RFC 7845 /
// RFC 3533), used purely for diagnostics: it lets received Opus packets be
// dumped to a file playable in any standard player (VLC, mpv, ffplay, ...)
// so the exact audio the bridge receives from a browser can be listened to
// directly, independent of anything downstream (Mumble relay, other
// clients). One packet per Ogg page — simpler than page segment-table
// packing and more than adequate for a diagnostic dump of small Opus
// packets.
type oggOpusWriter struct {
	w       io.Writer
	serial  uint32
	seq     uint32
	granule int64
	err     error
}

// newOggOpusWriter writes the two mandatory Opus-in-Ogg header packets
// (OpusHead, OpusTags — RFC 7845 §5.1/§5.2) and returns a writer ready to
// accept audio data packets via WritePacket.
func newOggOpusWriter(w io.Writer, serial uint32, channels uint8, inputSampleRate uint32) (*oggOpusWriter, error) {
	o := &oggOpusWriter{w: w, serial: serial}

	head := make([]byte, 19)
	copy(head[0:8], "OpusHead")
	head[8] = 1 // version
	head[9] = channels
	binary.LittleEndian.PutUint16(head[10:12], 0) // pre-skip; 0 is a valid (if imprecise) value
	binary.LittleEndian.PutUint32(head[12:16], inputSampleRate)
	binary.LittleEndian.PutUint16(head[16:18], 0) // output gain
	head[18] = 0                                  // channel mapping family 0: mono/stereo, no mapping table
	o.writePage(0x02, 0, [][]byte{head})           // BOS flag

	tags := make([]byte, 0, 8+4+len("mumble-webrtc-bridge diag"))
	tags = append(tags, "OpusTags"...)
	vendor := "mumble-webrtc-bridge diag"
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(vendor)))
	tags = append(tags, lenBuf[:]...)
	tags = append(tags, vendor...)
	binary.LittleEndian.PutUint32(lenBuf[:], 0) // zero user comments
	tags = append(tags, lenBuf[:]...)
	o.writePage(0x00, 0, [][]byte{tags})

	return o, o.err
}

// WritePacket writes one Opus data packet as its own Ogg page. sampleCount
// is the packet's duration in samples at 48kHz (Opus's fixed internal rate
// regardless of the original input rate), used to advance the granule
// position that players use for seeking/duration display.
func (o *oggOpusWriter) WritePacket(payload []byte, sampleCount int64) error {
	o.granule += sampleCount
	o.writePage(0x00, o.granule, [][]byte{payload})
	return o.err
}

// Close writes a final empty page with the EOS flag set, as RFC 3533
// expects every logical stream to end with one.
func (o *oggOpusWriter) Close() error {
	o.writePage(0x04, o.granule, nil)
	return o.err
}

func (o *oggOpusWriter) writePage(headerType byte, granule int64, packets [][]byte) {
	if o.err != nil {
		return
	}

	var segments []byte
	var data []byte
	for _, p := range packets {
		// Lacing: one 255-byte segment entry per full 255 bytes of packet
		// data, plus a final entry < 255 (0 if the packet is an exact
		// multiple of 255) marking the packet boundary. Our packets are
		// well under 255 bytes so this is always exactly one entry, but
		// handle the general case for correctness (e.g. a large frame or
		// the header packets).
		n := len(p)
		for n >= 255 {
			segments = append(segments, 255)
			n -= 255
		}
		segments = append(segments, byte(n))
		data = append(data, p...)
	}
	if len(segments) > 255 {
		// Would need multiple pages (continuation) to represent; none of
		// our packets are anywhere near this size, so treat it as a bug
		// rather than silently truncating/corrupting the stream.
		o.err = io.ErrShortBuffer
		return
	}

	page := make([]byte, 27+len(segments)+len(data))
	copy(page[0:4], "OggS")
	page[4] = 0 // version
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:14], uint64(granule))
	binary.LittleEndian.PutUint32(page[14:18], o.serial)
	binary.LittleEndian.PutUint32(page[18:22], o.seq)
	// page[22:26] CRC left zero for the checksum computation below
	page[26] = byte(len(segments))
	copy(page[27:27+len(segments)], segments)
	copy(page[27+len(segments):], data)

	binary.LittleEndian.PutUint32(page[22:26], oggCRC32(page))
	o.seq++

	_, o.err = o.w.Write(page)
}

// oggCRC32Table is for the CRC32 variant Ogg specifies (RFC 3533 §6):
// polynomial 0x04c11db7, not reflected, initial value 0, no final XOR —
// distinct from the common "CRC-32" (zlib/gzip) variant.
var oggCRC32Table = func() [256]uint32 {
	var t [256]uint32
	const poly = 0x04c11db7
	for i := range t {
		crc := uint32(i) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ poly
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	return t
}()

func oggCRC32(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = (crc << 8) ^ oggCRC32Table[byte(crc>>24)^b]
	}
	return crc
}
