package bridge

import (
	"github.com/hraban/opus"
	"layeh.com/gumble/gumble"
)

const opusCodecID = 4

// NewOpusCodec returns a gumble AudioCodec backed by libopus via hraban/opus.
// Register it once at startup: gumble.RegisterAudioCodec(4, bridge.NewOpusCodec())
func NewOpusCodec() gumble.AudioCodec {
	return opusCodec{}
}

type opusCodec struct{}

func (opusCodec) ID() int { return opusCodecID }

func (opusCodec) NewEncoder() gumble.AudioEncoder {
	enc, err := opus.NewEncoder(48000, 1, opus.AppVoIP)
	if err != nil {
		panic("opus encoder init: " + err.Error())
	}
	return &opusEncoder{enc: enc}
}

func (opusCodec) NewDecoder() gumble.AudioDecoder {
	dec, err := opus.NewDecoder(48000, 1)
	if err != nil {
		panic("opus decoder init: " + err.Error())
	}
	return &opusDecoder{dec: dec}
}

type opusEncoder struct{ enc *opus.Encoder }

func (e *opusEncoder) ID() int { return opusCodecID }

func (e *opusEncoder) Encode(pcm []int16, mframeSize, maxDataBytes int) ([]byte, error) {
	if mframeSize > len(pcm) {
		mframeSize = len(pcm)
	}
	data := make([]byte, maxDataBytes)
	n, err := e.enc.Encode(pcm[:mframeSize], data)
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}

func (e *opusEncoder) Reset() {
	if enc, err := opus.NewEncoder(48000, 1, opus.AppVoIP); err == nil {
		e.enc = enc
	}
}

type opusDecoder struct{ dec *opus.Decoder }

func (d *opusDecoder) ID() int { return opusCodecID }

func (d *opusDecoder) Decode(data []byte, frameSize int) ([]int16, error) {
	pcm := make([]int16, frameSize)
	n, err := d.dec.Decode(data, pcm)
	if err != nil {
		return nil, err
	}
	return pcm[:n], nil
}

func (d *opusDecoder) Reset() {
	if dec, err := opus.NewDecoder(48000, 1); err == nil {
		d.dec = dec
	}
}

// encodeOpus encodes a PCM int16 slice to Opus bytes using a dedicated encoder.
// Used by the audio mixer for the Mumble→Browser path.
func encodeOpus(enc *opus.Encoder, pcm []int16) ([]byte, error) {
	data := make([]byte, 1000)
	n, err := enc.Encode(pcm, data)
	if err != nil {
		return nil, err
	}
	return data[:n], nil
}
