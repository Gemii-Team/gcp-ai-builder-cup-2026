package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"kiwi/internal/platform/llm"
)

const (
	// defaultASRLanguage is sent with every chunk. On speech that mixes Thai and English
	// the model's own language detection decides per request and drops one of the two
	// languages (it turned "launch" into "ล็อต"); asking for English kept both scripts.
	defaultASRLanguage = "en"

	// asrCooldownWait is how long to wait after a 429 from the ASR. LiteLLM answers
	// 429 "try again in 60 seconds" for a minute after any failed request, from anyone,
	// and sends no Retry-After.
	asrCooldownWait = 65 * time.Second
	// asrCooldownRetries is how many times a chunk is sent again after such a 429.
	asrCooldownRetries = 3

	// asrRequestTimeout bounds one chunk. A chunk takes about 4 s alone, but the
	// deployment serves one request at a time and queues the others behind it.
	asrRequestTimeout = 180 * time.Second
)

type transcription struct {
	Text string `json:"text"`
}

// Transcribe implements llm.Client. The audio must be a PCM 16-bit WAV file, which
// this method checks itself before the first request: a body the ASR cannot decode is
// an HTTP 500 that stalls the deployment for everyone. It is cut into 20 s windows
// that are sent one at a time, because the server drops the end of anything much longer
// without saying so. The provider returns no timing and no speakers, so the
// entries have none; the text of the windows, joined, is split into sentences.
//
// A 429 is the ASR's cooldown: the chunk is sent again after 65 s, up to three times.
// A 500 is not repeated: it is deterministic, and starts a cooldown.
func (c *client) Transcribe(ctx context.Context, audio llm.Audio) ([]llm.TranscriptEntry, error) {
	if c.asrModel == "" {
		return nil, errors.New("openai: KIWI_ASR_MODEL is not set")
	}
	a, err := parseWAV(audio.Data)
	if err != nil {
		return nil, err
	}
	var texts []string
	for i, w := range a.windows() {
		text, err := c.transcribeChunk(ctx, fmt.Sprintf("chunk-%03d.wav", i+1), a.wav(w))
		if err != nil {
			return nil, fmt.Errorf("openai: transcribe window %d: %w", i+1, err)
		}
		if text = strings.TrimSpace(text); text != "" {
			texts = append(texts, text)
		}
	}
	var entries []llm.TranscriptEntry
	for _, s := range SplitSentences(strings.Join(texts, " ")) {
		entries = append(entries, llm.TranscriptEntry{Text: s})
	}
	return entries, nil
}

// transcribeChunk sends one window, waiting out the cooldown when the ASR is in one.
func (c *client) transcribeChunk(ctx context.Context, name string, file []byte) (string, error) {
	for attempt := 0; ; attempt++ {
		text, err := c.transcribeOnce(ctx, name, file)
		var he *llm.HTTPError
		if err == nil || !errors.As(err, &he) || he.Status != http.StatusTooManyRequests || attempt >= asrCooldownRetries {
			return text, err
		}
		if err := c.sleep(ctx, asrCooldownWait); err != nil {
			return "", err
		}
	}
}

func (c *client) transcribeOnce(ctx context.Context, name string, file []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("model", c.asrModel)
	_ = w.WriteField("language", defaultASRLanguage)
	part, err := w.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, name)},
		"Content-Type":        {"audio/wav"},
	})
	if err != nil {
		return "", fmt.Errorf("openai: build request: %w", err)
	}
	_, _ = part.Write(file)
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("openai: build request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, asrRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", errors.New("openai: build request")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.send(req, true)
	if err != nil {
		return "", err
	}
	var out transcription
	if err := decode(resp, &out); err != nil {
		return "", err
	}
	return out.Text, nil
}
