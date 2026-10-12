// Package wav is the one rule for what a recording must be before the transcription
// model is allowed to see it: a PCM 16-bit WAV file with audio in it. The model decodes
// the content, not the label on it, and a body it cannot decode (an MP3 or an MP4 that
// was called a WAV) is answered with an HTTP 500 that stops the shared transcription
// service for every user for a minute. Upload (docs), the job that sends a recording
// (meetings) and the model adapter (llm/openai) each check the bytes with Check, so a
// rule that changes changes everywhere at once.
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// ErrNotPCM16 is why Check refused a file. The error Check returns wraps it and says
// which rule the file broke.
var ErrNotPCM16 = errors.New("not a PCM 16-bit WAV file with audio in it")

// What Check accepts of a file's format.
const (
	// MinSampleRate and MaxSampleRate bound the sample rate, in Hz: telephone audio to
	// studio audio. A header that says 1 Hz, or 4 GHz, is not a recording.
	MinSampleRate = 8000
	MaxSampleRate = 96000
	// MinDuration is the least audio a recording has. A fraction of a second is no
	// meeting, and a file of a few frames is how a crafted one asks for many requests.
	MinDuration = 250 * time.Millisecond
)

// Format is what a file that passed Check says about its audio.
type Format struct {
	// Channels is 1 or 2.
	Channels uint16
	// SampleRate is in Hz, from MinSampleRate to MaxSampleRate.
	SampleRate uint32
	// DataOffset is where the audio starts in the checked bytes.
	DataOffset int64
	// DataLen is how many bytes of audio the file holds: what its data chunk says or, as a
	// streaming writer leaves it (a size of 0 or 0xFFFFFFFF), what is left of the file.
	// It lies inside the file, is at least MinDuration long, and is not rounded down to a
	// whole number of frames.
	DataLen int64
}

// FrameSize is the bytes of one sample frame: a 16-bit sample for each channel.
func (f Format) FrameSize() int64 { return int64(f.Channels) * 2 }

// Check accepts only what the transcription model can decode, and what is worth sending
// it: a RIFF/WAVE file whose fmt chunk says PCM (format tag 1; WAVE_FORMAT_EXTENSIBLE is
// refused), 16 bits per sample, mono or stereo, at a sample rate from MinSampleRate to
// MaxSampleRate, followed by a data chunk with at least MinDuration of audio. The fmt
// chunk must come before the data chunk. The bounds keep a crafted header from making
// the caller cut the audio into millions of requests, or send the model a sound it
// answers with an HTTP 500.
//
// The data chunk's size is checked against the file: a chunk that says it holds more
// bytes than the file has after its header is refused, as is a file cut off in the middle
// of its audio; one that says less (a chunk after the audio) is accepted, and the audio is
// what the chunk says.
//
// head is the first bytes of the file and total its whole size, so a caller that cannot
// afford to hold a long recording reads only the head: the chunks that come before the
// audio (fmt, then JUNK, bext, LIST) must lie in it, and a writer that put more than
// len(head) bytes of them first has its file refused. A caller that holds the whole file
// passes it as head with total = len(head). total must not be less than len(head).
//
// Chunk sizes are untrusted: one that runs past the head ends the scan instead of being
// followed.
func Check(head []byte, total int64) (Format, error) {
	if len(head) < 12 || string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return Format{}, fmt.Errorf("%w: no RIFF/WAVE header", ErrNotPCM16)
	}
	le := binary.LittleEndian
	var f Format
	for off := int64(12); off+8 <= int64(len(head)); {
		id, size := string(head[off:off+4]), int64(le.Uint32(head[off+4:off+8]))
		body := off + 8
		switch id {
		case "fmt ":
			if size < 16 || body+16 > int64(len(head)) {
				return Format{}, fmt.Errorf("%w: truncated fmt chunk", ErrNotPCM16)
			}
			tag, channels := le.Uint16(head[body:]), le.Uint16(head[body+2:])
			rate, bits := le.Uint32(head[body+4:]), le.Uint16(head[body+14:])
			if tag != 1 || (channels != 1 && channels != 2) || bits != 16 || rate < MinSampleRate || rate > MaxSampleRate {
				return Format{}, fmt.Errorf("%w: format %d, %d channels, %d bits, %d Hz", ErrNotPCM16, tag, channels, bits, rate)
			}
			f.Channels, f.SampleRate = channels, rate
		case "data":
			if f.Channels == 0 {
				return Format{}, fmt.Errorf("%w: the data chunk comes before the fmt chunk", ErrNotPCM16)
			}
			// A streaming writer leaves the size at 0 or 0xFFFFFFFF: the audio is the rest
			// of the file. Any other size is a claim the file has to bear out.
			audio := total - body
			if size != 0 && size != 0xFFFFFFFF {
				if size > audio {
					return Format{}, fmt.Errorf("%w: the data chunk says %d bytes and the file has %d after its header", ErrNotPCM16, size, audio)
				}
				audio = size
			}
			if audio < f.FrameSize() {
				return Format{}, fmt.Errorf("%w: the data chunk is empty", ErrNotPCM16)
			}
			if audio*1000 < int64(f.SampleRate)*f.FrameSize()*MinDuration.Milliseconds() {
				return Format{}, fmt.Errorf("%w: the audio is shorter than %s", ErrNotPCM16, MinDuration)
			}
			f.DataOffset, f.DataLen = body, audio
			return f, nil
		}
		// Chunks are padded to an even size.
		next := body + size + size&1
		if next > int64(len(head)) {
			break
		}
		off = next
	}
	if f.Channels == 0 {
		return Format{}, fmt.Errorf("%w: no fmt chunk", ErrNotPCM16)
	}
	return Format{}, fmt.Errorf("%w: no data chunk in the first %d bytes", ErrNotPCM16, len(head))
}
