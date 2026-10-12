package docs

import (
	"errors"
	"strings"
	"testing"

	"kiwi/internal/platform/errs"
	wavcheck "kiwi/internal/platform/wav" // aliased: this package's tests have a wav helper of their own
)

// The rule for a recording lives in platform/wav and is tested there. This proves the
// upload applies that rule, and nothing of its own: for the same bytes it refuses what
// wav.Check refuses, with the reason wav.Check gives, and accepts what it accepts.
func TestCompleteUpload_Recording_AppliesTheSharedWAVCheck(t *testing.T) {
	f := newFixture(t)
	ch := f.channel("public", f.alice)
	alice := f.user(f.alice)

	refused := map[string][]byte{
		"an mp3 labelled as a wav":    append([]byte("ID3\x03\x00\x00\x00\x00\x0a\x3f"), make([]byte, 4096)...),
		"24-bit samples":              wav(1, 1, 48000, 24),
		"WAVE_FORMAT_EXTENSIBLE":      wav(0xFFFE, 2, 48000, 16),
		"three channels":              wav(1, 3, 16000, 16),
		"no audio":                    riff("WAVE", chunk("fmt ", fmtBody(1, 1, 16000, 16)), chunk("data", nil)),
		"the data before the fmt":     riff("WAVE", chunk("data", make([]byte, 16000)), chunk("fmt ", fmtBody(1, 1, 16000, 16))),
		"a sample rate of 1 Hz":       wav(1, 1, 1, 16),
		"a sample rate of 96001 Hz":   wav(1, 1, 96001, 16),
		"a tenth of a second":         wavData(3200, 3200),
		"a data length beyond a file": wavData(1<<20, 16000),
	}
	for name, body := range refused {
		_, want := wavcheck.Check(body, int64(len(body)))
		if !errors.Is(want, wavcheck.ErrNotPCM16) {
			t.Fatalf("%s: wav.Check = %v, want it to refuse the body this test says is refused", name, want)
		}
		id := f.upload(alice, ch, "m.wav", "audio/wav", recording, body)
		_, err := f.svc.CompleteUpload(t.Context(), alice, id)
		if !errors.Is(err, errs.ErrInvalid) || !strings.Contains(err.Error(), reasonUnsupportedAudio) || !strings.Contains(err.Error(), want.Error()) {
			t.Errorf("%s: CompleteUpload = %v, want unsupported_audio with wav.Check's reason %q", name, err, want)
		}
	}

	accepted := map[string][]byte{
		"a plain file":           wav(1, 1, 16000, 16),
		"stereo at 44.1 kHz":     wav(1, 2, 44100, 16),
		"96 kHz":                 wav(1, 1, 96000, 16),
		"8 kHz":                  wav(1, 1, 8000, 16),
		"a LIST chunk in front":  wav(1, 1, 16000, 16, chunk("LIST", make([]byte, 100))),
		"a streaming writer's 0": wavData(0, 16000),
		"a JUNK chunk of 20 KiB": wavData(16000, 16000, chunk("JUNK", make([]byte, 20<<10))),
	}
	for name, body := range accepted {
		if _, err := wavcheck.Check(body, int64(len(body))); err != nil {
			t.Fatalf("%s: wav.Check = %v, want it to accept the body this test says is accepted", name, err)
		}
		id := f.upload(alice, ch, "m.wav", "audio/wav", recording, body)
		if _, err := f.svc.CompleteUpload(t.Context(), alice, id); err != nil {
			t.Errorf("%s: CompleteUpload = %v, want it accepted as wav.Check accepts it", name, err)
		}
	}
}
