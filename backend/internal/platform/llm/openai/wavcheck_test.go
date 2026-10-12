package openai

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"kiwi/internal/platform/llm"
	wavcheck "kiwi/internal/platform/wav" // aliased: this package's tests have a wav helper of their own
)

// The rule for a recording lives in platform/wav and is tested there. This proves the
// adapter applies that rule before it makes a request, and hands the model the audio
// wav.Check found: for the same bytes it refuses what wav.Check refuses, with its reason,
// and sends no request.
func TestTranscribe_AppliesTheSharedWAVCheck(t *testing.T) {
	data := tone(16000)
	refused := map[string][]byte{
		"an mp3 labelled as a wav": append([]byte("ID3\x04\x00\x00"), make([]byte, 100)...),
		"24-bit samples":           wav(16000, 1, 24, data, nil, nil),
		"WAVE_FORMAT_EXTENSIBLE":   wavTagged(0xFFFE, 16000, 2, 16, data, nil, nil, uint32(len(data))),
		"three channels":           wav(16000, 3, 16, data, nil, nil),
		"no audio":                 wav(16000, 1, 16, nil, nil, nil),
		"a sample rate of 1 Hz":    wav(1, 1, 16, data, nil, nil),
		"a sample rate of 96001":   wav(96001, 1, 16, data, nil, nil),
		"a tenth of a second":      wav(16000, 1, 16, tone(1600), nil, nil),
		"a data length beyond it":  wavTagged(1, 16000, 1, 16, data, nil, nil, uint32(len(data))+1000),
	}
	for name, audio := range refused {
		_, want := wavcheck.Check(audio, int64(len(audio)))
		if !errors.Is(want, wavcheck.ErrNotPCM16) {
			t.Fatalf("%s: wav.Check = %v, want it to refuse the audio this test says is refused", name, want)
		}
		cn := newCanned(transcribeReply("never"))
		_, err := newClient(t, cn).Transcribe(t.Context(), llm.Audio{Data: audio, MIME: "audio/wav", Name: "m.wav"})
		if !errors.Is(err, llm.ErrUnsupportedByProvider) || !errors.Is(err, wavcheck.ErrNotPCM16) || !strings.Contains(err.Error(), want.Error()) {
			t.Errorf("%s: Transcribe = %v, want ErrUnsupportedByProvider with wav.Check's reason %q", name, err, want)
		}
		if cn.count() != 0 {
			t.Errorf("%s: %d requests sent, want none", name, cn.count())
		}
	}

	// What wav.Check accepts goes to the model as the audio it found: a chunk after the
	// audio does not leak into it, and a streaming writer's audio is what the file holds.
	for name, audio := range map[string][]byte{
		"a trailing chunk":   wav(16000, 1, 16, data, nil, [][]byte{append([]byte("LIST"), append([]byte{4, 0, 0, 0}, []byte("tail")...)...)}),
		"a streaming writer": wavTagged(1, 16000, 1, 16, data, nil, nil, 0xFFFFFFFF),
	} {
		f, err := wavcheck.Check(audio, int64(len(audio)))
		if err != nil {
			t.Fatalf("%s: wav.Check = %v", name, err)
		}
		cn := newCanned(transcribeReply("Hello."))
		if _, err := newClient(t, cn).Transcribe(t.Context(), llm.Audio{Data: audio, MIME: "audio/wav", Name: "m.wav"}); err != nil {
			t.Fatalf("%s: Transcribe = %v", name, err)
		}
		_, _, file := cn.only(t, "/v1/audio/transcriptions").form(t)
		if got := pcmOf(t, file, f.SampleRate, f.Channels); !bytes.Equal(got, audio[f.DataOffset:f.DataOffset+f.DataLen]) {
			t.Errorf("%s: the chunk holds %d bytes of audio, want the %d wav.Check found at %d", name, len(got), f.DataLen, f.DataOffset)
		}
	}
}
