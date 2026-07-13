package bridge

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestOggOpusWriterStructure validates the container structure produced by
// newOggOpusWriter/WritePacket/Close is self-consistent: correct "OggS"
// capture pattern per page, correct CRC (recomputed independently and
// compared against what was written), monotonically increasing page
// sequence numbers, and a final page with the EOS flag set. This can't
// confirm the file actually plays (no player available here) but it does
// catch structural/binary-format bugs.
func TestOggOpusWriterStructure(t *testing.T) {
	var buf bytes.Buffer
	ow, err := newOggOpusWriter(&buf, 12345, 2, 48000)
	if err != nil {
		t.Fatalf("newOggOpusWriter: %v", err)
	}
	for i := 0; i < 5; i++ {
		payload := []byte{0xfc, byte(i), byte(i + 1), byte(i + 2)}
		if err := ow.WritePacket(payload, 960); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
	}
	if err := ow.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data := buf.Bytes()
	pages := parseOggPages(t, data)

	wantPages := 2 /* OpusHead, OpusTags */ + 5 /* audio packets */ + 1 /* EOS */
	if len(pages) != wantPages {
		t.Fatalf("got %d pages, want %d", len(pages), wantPages)
	}

	for i, p := range pages {
		if p.serial != 12345 {
			t.Errorf("page %d: serial = %d, want 12345", i, p.serial)
		}
		if p.seq != uint32(i) {
			t.Errorf("page %d: seq = %d, want %d", i, p.seq, i)
		}
	}

	if pages[0].headerType&0x02 == 0 {
		t.Error("first page missing BOS flag")
	}
	last := pages[len(pages)-1]
	if last.headerType&0x04 == 0 {
		t.Error("last page missing EOS flag")
	}

	if !bytes.HasPrefix(pages[0].payload, []byte("OpusHead")) {
		t.Errorf("first page payload = %q, want OpusHead prefix", pages[0].payload)
	}
	if !bytes.HasPrefix(pages[1].payload, []byte("OpusTags")) {
		t.Errorf("second page payload = %q, want OpusTags prefix", pages[1].payload)
	}

	// Granule position should accumulate by 960 samples per audio packet
	// (the EOS page repeats the last granule, per spec, since it carries
	// no new packet).
	for i := 0; i < 5; i++ {
		want := int64(960 * (i + 1))
		got := pages[2+i].granule
		if got != want {
			t.Errorf("audio page %d: granule = %d, want %d", i, got, want)
		}
	}
}

type parsedPage struct {
	headerType byte
	granule    int64
	serial     uint32
	seq        uint32
	payload    []byte
}

// parseOggPages is a minimal, independent Ogg page parser (deliberately
// not sharing code with oggopus.go) used only to verify the writer's
// output — including recomputing each page's CRC from scratch the same
// way an external player would, rather than trusting the writer's own
// internal state.
func parseOggPages(t *testing.T, data []byte) []parsedPage {
	t.Helper()
	var pages []parsedPage
	for len(data) > 0 {
		if len(data) < 27 || string(data[0:4]) != "OggS" {
			t.Fatalf("bad page header at offset %d", len(data))
		}
		headerType := data[5]
		granule := int64(binary.LittleEndian.Uint64(data[6:14]))
		serial := binary.LittleEndian.Uint32(data[14:18])
		seq := binary.LittleEndian.Uint32(data[18:22])
		storedCRC := binary.LittleEndian.Uint32(data[22:26])
		numSegs := int(data[26])
		if len(data) < 27+numSegs {
			t.Fatalf("truncated segment table")
		}
		segTable := data[27 : 27+numSegs]
		payloadLen := 0
		for _, s := range segTable {
			payloadLen += int(s)
		}
		pageLen := 27 + numSegs + payloadLen
		if len(data) < pageLen {
			t.Fatalf("truncated page data")
		}
		page := make([]byte, pageLen)
		copy(page, data[:pageLen])
		binary.LittleEndian.PutUint32(page[22:26], 0)
		if gotCRC := oggCRC32(page); gotCRC != storedCRC {
			t.Errorf("page at seq=%d: CRC mismatch: stored=%x recomputed=%x", seq, storedCRC, gotCRC)
		}

		pages = append(pages, parsedPage{
			headerType: headerType,
			granule:    granule,
			serial:     serial,
			seq:        seq,
			payload:    data[27+numSegs : pageLen],
		})
		data = data[pageLen:]
	}
	return pages
}
