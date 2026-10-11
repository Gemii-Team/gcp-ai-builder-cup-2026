package openai

import (
	"encoding/binary"
	"fmt"

	"kiwi/internal/platform/llm"
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

// parseWAV reads the audio out of a RIFF/WAVE file whose fmt chunk says PCM, 16 bits,
// mono or stereo, at any sample rate: what upload accepts and what the ASR decodes.
// The ASR answers anything else with an HTTP 500 that puts the deployment into a
// cooldown for everybody, so it is refused here, before a request. A data chunk whose
// size is 0 or 0xFFFFFFFF, as streaming writers leave it, runs to the end of the file.
func parseWAV(b []byte) (wavAudio, error) {
	bad := func(why string) (wavAudio, error) {
		return wavAudio{}, fmt.Errorf("%w: the audio is not a PCM 16-bit WAV file: %s", llm.ErrUnsupportedByProvider, why)
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return bad("no RIFF/WAVE header")
	}
	le := binary.LittleEndian
	var a wavAudio
	for off := int64(12); off+8 <= int64(len(b)); {
		id, size := string(b[off:off+4]), int64(le.Uint32(b[off+4:off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > int64(len(b)) {
				return bad("truncated fmt chunk")
			}
			format, channels := le.Uint16(b[body:]), le.Uint16(b[body+2:])
			rate, bits := le.Uint32(b[body+4:]), le.Uint16(b[body+14:])
			if format != 1 || (channels != 1 && channels != 2) || bits != 16 || rate == 0 {
				return bad(fmt.Sprintf("format %d, %d channels, %d bits, %d Hz", format, channels, bits, rate))
			}
			a.rate, a.channels = rate, channels
		case "data":
			if a.channels == 0 {
				return bad("the data chunk comes before the fmt chunk")
			}
			n := int64(len(b)) - body
			if size != 0 && size != 0xFFFFFFFF {
				n = min(size, n)
			}
			n -= n % int64(a.frame())
			if n == 0 {
				return bad("the data chunk holds no audio")
			}
			a.data = b[body : body+n]
			return a, nil
		}
		// Chunks are padded to an even size. size is untrusted: a chunk that runs past
		// the end of the file ends the scan.
		next := body + size + size&1
		if next > int64(len(b)) {
			break
		}
		off = next
	}
	if a.channels == 0 {
		return bad("no fmt chunk")
	}
	return bad("no data chunk")
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
