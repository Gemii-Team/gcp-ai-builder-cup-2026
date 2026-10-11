package wav

import (
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
)

// chunk is a RIFF chunk: its id, its size, its body, padded to an even length.
func chunk(id string, body []byte) []byte {
	b := append([]byte(id), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...)
	b = append(b, body...)
	if len(body)%2 == 1 {
		b = append(b, 0)
	}
	return b
}

// sized is a chunk whose size field says size, whatever the body holds.
func sized(id string, size uint32, body []byte) []byte {
	return append(append([]byte(id), binary.LittleEndian.AppendUint32(nil, size)...), body...)
}

// fmtBody is the body of a fmt chunk.
func fmtBody(tag, channels uint16, rate uint32, bits uint16) []byte {
	blockAlign := channels * bits / 8
	b := binary.LittleEndian.AppendUint16(nil, tag)
	b = binary.LittleEndian.AppendUint16(b, channels)
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = binary.LittleEndian.AppendUint32(b, rate*uint32(blockAlign))
	b = binary.LittleEndian.AppendUint16(b, blockAlign)
	return binary.LittleEndian.AppendUint16(b, bits)
}

// riff wraps chunks in a RIFF container of the given form type.
func riff(form string, chunks ...[]byte) []byte {
	body := []byte(form)
	for _, c := range chunks {
		body = append(body, c...)
	}
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...)
}

// half is the audio of half a second of mono 16 kHz PCM16, in bytes.
const half = 16000

// pcm is the chunks of a mono 16 kHz PCM16 file with n bytes of audio, after anything
// else the test puts in front of the fmt chunk.
func pcm(n int, before ...[]byte) [][]byte {
	return append(slices.Clone(before), chunk("fmt ", fmtBody(1, 1, 16000, 16)), chunk("data", make([]byte, n)))
}

// at is a file of the given format with n bytes of audio.
func at(tag, channels uint16, rate uint32, bits uint16, n int) []byte {
	return riff("WAVE", chunk("fmt ", fmtBody(tag, channels, rate, bits)), chunk("data", make([]byte, n)))
}

func pad(head []byte) []byte { return append(head, make([]byte, 4096)...) }

func TestCheck_AcceptsWhatTheModelCanDecode(t *testing.T) {
	listBefore := chunk("LIST", make([]byte, 5000))
	cases := []struct {
		name string
		file []byte
		want Format
	}{
		{"a plain mono 16 kHz file", riff("WAVE", pcm(half)...), Format{1, 16000, 44, half}},
		{"stereo at 44.1 kHz", at(1, 2, 44100, 16, 50000), Format{2, 44100, 44, 50000}},
		{"8 kHz, the lowest rate", at(1, 1, 8000, 16, 8000), Format{1, 8000, 44, 8000}},
		{"96 kHz, the highest rate", at(1, 1, 96000, 16, 96000), Format{1, 96000, 44, 96000}},
		{"48 kHz", at(1, 1, 48000, 16, 48000), Format{1, 48000, 44, 48000}},
		// 0.25 s is enough; a frame less is not (see the refused cases).
		{"exactly 0.25 s of audio", at(1, 1, 16000, 16, 8000), Format{1, 16000, 44, 8000}},
		{"exactly 0.25 s of stereo audio", at(1, 2, 44100, 16, 44100), Format{2, 44100, 44, 44100}},
		{"an odd byte after the last frame", riff("WAVE", pcm(half+1)...), Format{1, 16000, 44, half + 1}},
		{"a LIST chunk before the fmt chunk", riff("WAVE", pcm(half, listBefore)...), Format{1, 16000, 44 + int64(len(listBefore)), half}},
		{"a JUNK chunk of 20 KiB before the fmt chunk", riff("WAVE", pcm(half, chunk("JUNK", make([]byte, 20<<10)))...), Format{1, 16000, 44 + 8 + 20<<10, half}},
		{"an odd-sized chunk, padded, before the data", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), chunk("LIST", []byte("abc")), chunk("data", make([]byte, 8000))), Format{1, 16000, 44 + 12, 8000}},
		{"a fmt chunk with an extension", riff("WAVE", chunk("fmt ", append(fmtBody(1, 1, 16000, 16), 0, 0)), chunk("data", make([]byte, 8000))), Format{1, 16000, 46, 8000}},
		{"a trailing chunk after the data", riff("WAVE", append(pcm(half), chunk("LIST", []byte("tail")))...), Format{1, 16000, 44, half}},
		// A streaming writer does not know the length when it writes the header.
		{"a data size of 0xFFFFFFFF", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("data", 0xFFFFFFFF, make([]byte, half))), Format{1, 16000, 44, half}},
		{"a data size of 0", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("data", 0, make([]byte, half))), Format{1, 16000, 44, half}},
		{"chunks between the fmt and the data chunk", riff("WAVE", chunk("fmt ", fmtBody(1, 2, 44100, 16)), chunk("LIST", make([]byte, 5000)), chunk("data", make([]byte, 50000))), Format{2, 44100, 44 + 5008, 50000}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Check(c.file, int64(len(c.file)))
			if err != nil {
				t.Fatalf("Check = %v, want it accepted", err)
			}
			if got != c.want {
				t.Errorf("Format = %+v, want %+v", got, c.want)
			}
			if got.DataOffset+got.DataLen > int64(len(c.file)) {
				t.Errorf("the audio [%d, +%d) lies outside the %d bytes", got.DataOffset, got.DataLen, len(c.file))
			}
		})
	}
}

func TestCheck_RefusesWhatTheModelCannotDecode(t *testing.T) {
	cases := []struct {
		name string
		file []byte
		why  string // part of the message that names the broken rule
	}{
		// Not a RIFF/WAVE file at all, whatever it is called.
		{"nothing", nil, "no RIFF/WAVE header"},
		{"a header only", []byte("RIFF"), "no RIFF/WAVE header"},
		{"eleven bytes", []byte("RIFF\x24\x00\x00\x00WAV"), "no RIFF/WAVE header"},
		{"an mp3 with an ID3 tag", pad([]byte("ID3\x03\x00\x00\x00\x00\x0a\x3f")), "no RIFF/WAVE header"},
		{"mp3 frames", pad([]byte{0xFF, 0xFB, 0x90, 0x00}), "no RIFF/WAVE header"},
		{"an mp4", pad([]byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")), "no RIFF/WAVE header"},
		{"an m4a", pad([]byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00M4A mp42isom")), "no RIFF/WAVE header"},
		{"ogg", pad([]byte("OggS\x00\x02\x00\x00")), "no RIFF/WAVE header"},
		{"flac", pad([]byte("fLaC\x00\x00\x00\x22")), "no RIFF/WAVE header"},
		{"webm", pad([]byte{0x1A, 0x45, 0xDF, 0xA3}), "no RIFF/WAVE header"},
		{"html", []byte("<html><body>not audio</body></html>"), "no RIFF/WAVE header"},
		{"riff but not wave", riff("AVI ", chunk("LIST", make([]byte, 100))), "no RIFF/WAVE header"},
		{"wave inside something that is not riff", append([]byte("RIFX\x24\x00\x00\x00WAVE"), make([]byte, 100)...), "no RIFF/WAVE header"},

		// A WAV whose audio the model does not decode.
		{"float32", at(3, 1, 16000, 32, half), "format 3, 1 channels, 32 bits"},
		{"mu-law", at(7, 1, 8000, 8, half), "format 7"},
		{"a-law", at(6, 1, 8000, 8, half), "format 6"},
		{"8-bit pcm", at(1, 1, 8000, 8, half), "8 bits"},
		{"24-bit pcm", at(1, 1, 48000, 24, half), "24 bits"},
		{"32-bit pcm", at(1, 1, 48000, 32, half), "32 bits"},
		{"WAVE_FORMAT_EXTENSIBLE at 16 bits", at(0xFFFE, 2, 48000, 16, half), "format 65534"},
		{"zero channels", at(1, 0, 16000, 16, half), "0 channels"},
		{"three channels", at(1, 3, 16000, 16, half), "3 channels"},
		{"six channels", at(1, 6, 48000, 16, half), "6 channels"},

		// A sample rate that is no recording's: the rate decides how the audio is cut into
		// requests, so a crafted one must not reach the cutting.
		{"a sample rate of 0 Hz", at(1, 1, 0, 16, half), "0 Hz"},
		{"a sample rate of 1 Hz", at(1, 1, 1, 16, half), "1 Hz"},
		{"a sample rate of 7999 Hz", at(1, 1, 7999, 16, half), "7999 Hz"},
		{"a sample rate of 96001 Hz", at(1, 1, 96001, 16, half), "96001 Hz"},
		{"a sample rate of 4 GHz", at(1, 1, 0xFFFFFFFF, 16, half), "4294967295 Hz"},

		// Broken or cut-off structure.
		{"a wave without a fmt chunk", riff("WAVE", chunk("data", make([]byte, 100))), "the data chunk comes before the fmt chunk"},
		{"nothing after the wave marker", []byte("RIFF\x04\x00\x00\x00WAVE"), "no fmt chunk"},
		{"a fmt chunk cut short", []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00"), "truncated fmt chunk"},
		{"a fmt chunk that says it is 8 bytes", riff("WAVE", chunk("fmt ", make([]byte, 8)), chunk("data", make([]byte, 100))), "truncated fmt chunk"},
		{"the data chunk before the fmt chunk", riff("WAVE", chunk("data", make([]byte, half)), chunk("fmt ", fmtBody(1, 1, 16000, 16))), "the data chunk comes before the fmt chunk"},
		{"a fmt chunk beyond the first chunks", riff("WAVE", chunk("data", make([]byte, 8000)), chunk("fmt ", fmtBody(1, 1, 16000, 16))), "the data chunk comes before the fmt chunk"},
		{"a chunk size past the end", append([]byte("RIFF\x24\x00\x00\x00WAVE"), []byte("JUNK\xff\xff\xff\xff")...), "no fmt chunk"},
		{"a chunk size past the end after the fmt chunk", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("JUNK", 0xFFFFFFF0, make([]byte, 100))), "no data chunk"},
		{"the fmt chunk and no data chunk", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), chunk("LIST", make([]byte, 100))), "no data chunk"},
		{"a header cut off before the data chunk", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)))[:36], "no data chunk"},

		// A header and no audio, or too little of it to be a recording.
		{"a header and no audio, 44 bytes", riff("WAVE", pcm(0)...), "the data chunk is empty"},
		{"a data chunk of less than a mono frame", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("data", 1, []byte{1})), "the data chunk is empty"},
		{"a data chunk of less than a stereo frame", riff("WAVE", chunk("fmt ", fmtBody(1, 2, 16000, 16)), sized("data", 3, []byte{1, 2, 3})), "the data chunk is empty"},
		{"a data chunk that is declared empty, after a LIST", riff("WAVE", pcm(0, chunk("LIST", make([]byte, 20)))...), "the data chunk is empty"},
		{"one frame of audio", at(1, 1, 16000, 16, 2), "shorter than 250ms"},
		{"100 ms of audio", at(1, 1, 16000, 16, 3200), "shorter than 250ms"},
		{"100 ms of stereo audio", at(1, 2, 44100, 16, 17640), "shorter than 250ms"},
		{"a frame short of 0.25 s", at(1, 1, 16000, 16, 7998), "shorter than 250ms"},
		{"a frame short of 0.25 s at 96 kHz", at(1, 1, 96000, 16, 47998), "shorter than 250ms"},

		// A data chunk that says more than the file has: a lie, or a file cut off.
		{"a data length beyond the file", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("data", 1<<20, make([]byte, half))), "says 1048576 bytes and the file has 16000"},
		{"a data length one byte beyond the file", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("data", half+1, make([]byte, half))), "says 16001 bytes and the file has 16000"},
		{"a data length of 4 GiB less one", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("data", 0xFFFFFFFE, make([]byte, half))), "says 4294967294 bytes"},
		{"a data chunk declared and cut off", riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("data", 3200, nil)), "says 3200 bytes and the file has 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Check(c.file, int64(len(c.file)))
			if !errors.Is(err, ErrNotPCM16) {
				t.Fatalf("Check = %+v, %v, want ErrNotPCM16", got, err)
			}
			if !strings.Contains(err.Error(), c.why) {
				t.Errorf("error = %q, want it to say %q", err, c.why)
			}
			if got != (Format{}) {
				t.Errorf("Format = %+v for a refused file, want none", got)
			}
		})
	}
}

// A caller that cannot hold a long recording reads only its head and the file's size.
func TestCheck_HeadAndTotal(t *testing.T) {
	const headBytes = 64 << 10
	whole := riff("WAVE", pcm(half)...)

	// The whole file is the head: 44 bytes of header, then the audio.
	if f, err := Check(whole, int64(len(whole))); err != nil || f.DataOffset != 44 || f.DataLen != half {
		t.Fatalf("Check(whole) = %+v, %v, want the audio at 44, %d bytes long", f, err, half)
	}
	// Only the header is read, and the size says there is audio behind it.
	if f, err := Check(whole[:44], int64(len(whole))); err != nil || f.DataOffset != 44 || f.DataLen != half {
		t.Errorf("Check(header, total) = %+v, %v, want the audio the size accounts for", f, err)
	}
	// The same header with nothing behind it: the chunk says more than the file has.
	if _, err := Check(whole[:44], 44); !errors.Is(err, ErrNotPCM16) || !strings.Contains(err.Error(), "says 16000 bytes and the file has 0") {
		t.Errorf("Check(header, 44) = %v, want a refusal: the data chunk says more than the file has", err)
	}
	// A data size longer than the file is a lie whatever part of the file is read.
	over := riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), sized("data", 1<<30, make([]byte, half)))
	if _, err := Check(over[:60], int64(len(over))); !errors.Is(err, ErrNotPCM16) || !strings.Contains(err.Error(), "says 1073741824 bytes") {
		t.Errorf("Check(over, head) = %v, want a refusal: the data chunk lies beyond the file", err)
	}

	// 70 KiB of padding between the fmt chunk and the audio: the audio is there, but not in
	// the head. Padding in front of the fmt chunk costs the fmt chunk as well.
	far := riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), chunk("JUNK", make([]byte, 70<<10)), chunk("data", make([]byte, half)))
	if _, err := Check(far, int64(len(far))); err != nil {
		t.Errorf("Check(far, whole) = %v, want it accepted when the whole file is looked at", err)
	}
	_, err := Check(far[:headBytes], int64(len(far)))
	if !errors.Is(err, ErrNotPCM16) || !strings.Contains(err.Error(), "no data chunk in the first 65536 bytes") {
		t.Errorf("Check(far, head) = %v, want a refusal that names how much was read", err)
	}
	farFmt := riff("WAVE", pcm(half, chunk("JUNK", make([]byte, 70<<10)))...)
	if _, err := Check(farFmt[:headBytes], int64(len(farFmt))); !errors.Is(err, ErrNotPCM16) || !strings.Contains(err.Error(), "no fmt chunk") {
		t.Errorf("Check(farFmt, head) = %v, want a refusal: no fmt chunk in the head", err)
	}
}

// Whatever a client uploads is checked, so no prefix of a good file, and no file with a
// size field turned into nonsense, may do anything but be accepted or refused.
func TestCheck_NeverPanicsOnDamagedFiles(t *testing.T) {
	good := riff("WAVE", pcm(8000, chunk("LIST", []byte("abcde")))...)
	for n := range len(good) + 1 {
		_, _ = Check(good[:n], int64(len(good)))
		_, _ = Check(good[:n], int64(n))
	}
	for at := 4; at+4 <= len(good); at++ {
		for _, v := range []uint32{0, 1, 0x7FFFFFFF, 0xFFFFFFFE, 0xFFFFFFFF} {
			bad := slices.Clone(good)
			binary.LittleEndian.PutUint32(bad[at:], v)
			f, err := Check(bad, int64(len(bad)))
			if err == nil && f.DataOffset+f.DataLen > int64(len(bad)) {
				t.Fatalf("a size field of %#x at %d gave the audio [%d, +%d) outside the %d bytes", v, at, f.DataOffset, f.DataLen, len(bad))
			}
		}
	}
}

// What is accepted can be cut into requests: a file that passes has a rate that makes
// windows of a sane size and enough audio to be worth one.
func TestCheck_AcceptedFilesHaveABoundedFormat(t *testing.T) {
	for _, rate := range []uint32{MinSampleRate, 11025, 16000, 22050, 44100, 48000, 88200, MaxSampleRate} {
		for _, channels := range []uint16{1, 2} {
			frame := int(channels) * 2
			n := (int(rate) + 3) / 4 * frame // 0.25 s, to a whole frame
			file := at(1, channels, rate, 16, n)
			f, err := Check(file, int64(len(file)))
			if err != nil {
				t.Errorf("%d Hz, %d channels, 0.25 s: %v", rate, channels, err)
				continue
			}
			if f.SampleRate < MinSampleRate || f.SampleRate > MaxSampleRate || f.DataLen < int64(n) {
				t.Errorf("Format = %+v, want a rate in [%d, %d] and at least %d bytes of audio", f, MinSampleRate, MaxSampleRate, n)
			}
		}
	}
}

func TestFormat_FrameSize(t *testing.T) {
	if got := (Format{Channels: 1}).FrameSize(); got != 2 {
		t.Errorf("mono frame = %d, want 2", got)
	}
	if got := (Format{Channels: 2}).FrameSize(); got != 4 {
		t.Errorf("stereo frame = %d, want 4", got)
	}
}
