package openai

import (
	"encoding/binary"
	"fmt"

	"kiwi/internal/platform/llm"
	wavcheck "kiwi/internal/platform/wav" // aliased: the package's tests have a wav helper of their own
)

const (
	// asrWindowSeconds is how long a piece of audio is that goes to the ASR in one
	// request. The server cuts what it is given silently at about 700 characters of
	// transcript (some 40 s of speech); 20 s was complete in every sample, 30 s is the
	// most that was.
	asrWindowSeconds = 20
	// asrMinTailSeconds: a remainder shorter than this joins the window before it, so
	// that no request carries a fraction of a second.
	asrMinTailSeconds = 1
)

// wavAudio is the audio of a PCM 16-bit WAV file.
type wavAudio struct {
	rate     uint32
	channels uint16
	data     []byte // whole frames, as stored
}

func (a wavAudio) frame() int { return int(a.channels) * 2 }

// parseWAV reads the audio out of a PCM 16-bit WAV file. wav.Check is the rule for what
// that is (the one upload applies and the ASR decodes): the ASR answers anything else
// with an HTTP 500 that puts the deployment into a cooldown for everybody, so it is
// refused here, before a request.
func parseWAV(b []byte) (wavAudio, error) {
	f, err := wavcheck.Check(b, int64(len(b)))
	if err != nil {
		return wavAudio{}, fmt.Errorf("%w: %w", llm.ErrUnsupportedByProvider, err)
	}
	n := f.DataLen - f.DataLen%f.FrameSize()
	return wavAudio{rate: f.SampleRate, channels: f.Channels, data: b[f.DataOffset : f.DataOffset+n]}, nil
}

// windows cuts the audio into asrWindowSeconds pieces; the last is shorter, unless
// that would be under asrMinTailSeconds, when it joins the piece before.
func (a wavAudio) windows() [][]byte {
	win := int(a.rate) * asrWindowSeconds * a.frame()
	tail := int(a.rate) * asrMinTailSeconds * a.frame()
	var out [][]byte
	for off := 0; off < len(a.data); {
		end := min(off+win, len(a.data))
		if rest := len(a.data) - end; rest > 0 && rest < tail {
			end = len(a.data)
		}
		out = append(out, a.data[off:end])
		off = end
	}
	return out
}

// wav wraps pcm in a canonical 44-byte-header WAV file of the format of a.
func (a wavAudio) wav(pcm []byte) []byte {
	le := binary.LittleEndian
	out := make([]byte, 0, 44+len(pcm))
	out = append(out, "RIFF"...)
	out = le.AppendUint32(out, uint32(36+len(pcm)))
	out = append(out, "WAVEfmt "...)
	out = le.AppendUint32(out, 16)
	out = le.AppendUint16(out, 1)
	out = le.AppendUint16(out, a.channels)
	out = le.AppendUint32(out, a.rate)
	out = le.AppendUint32(out, a.rate*uint32(a.frame()))
	out = le.AppendUint16(out, uint16(a.frame()))
	out = le.AppendUint16(out, 16)
	out = append(out, "data"...)
	out = le.AppendUint32(out, uint32(len(pcm)))
	return append(out, pcm...)
}
