package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sync"
	"testing"
	"time"

	"kiwi/internal/platform/blob"
	"kiwi/internal/testkit/fakeoidc"
)

// uploadContractDir holds one JSON file per way CreateUpload and CompleteUpload refuse
// a file, written by the composed server like the StartMeeting ones: the status, the
// content type and the exact body. The web reads the same files
// (apps/web/src/features/channel/uploadContract.test.tsx) and feeds each through its own
// transport, generated client and upload hook, so the reason a person is told is the
// one the server sent and not one a test wrote out by hand.
const uploadContractDir = "../../../packages/api-client/testdata/upload-errors"

// shrinksHead is an object store whose Head tells a smaller size than the object has for
// the keys that were armed, which is what CompleteUpload sees of an object that is
// written again between its Head and its Read.
type shrinksHead struct {
	blob.Store
	mu    sync.Mutex
	armed map[string]bool
}

func (s *shrinksHead) arm(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed[key] = true
}

func (s *shrinksHead) Head(ctx context.Context, key string) (blob.Attrs, error) {
	attrs, err := s.Store.Head(ctx, key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && s.armed[key] && attrs.Size > 1 {
		attrs.Size--
	}
	return attrs, err
}

// refusal is what a Connect unary failure answers with: the code and the text.
type refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (a contractAnswer) refusal(t *testing.T) refusal {
	t.Helper()
	var r refusal
	if err := json.Unmarshal(a.Body, &r); err != nil {
		t.Fatalf("the answer %s is not a Connect error: %v", a.Body, err)
	}
	return r
}

// The bytes the composed server writes for every refusal of an upload equal the committed
// fixtures the web is tested against, and each names the reason the person is told, in
// the text the web reads it from: a refusal whose text is only "invalid" is the generic
// notice in the browser for every file.
func TestContract_UploadRefusals_MatchTheFixturesTheWebReads(t *testing.T) {
	var shrink *shrinksHead
	r := newRig(t, withOption(WithBlobWrap(func(s blob.Store) blob.Store {
		shrink = &shrinksHead{Store: s, armed: map[string]bool{}}
		return shrink
	})))
	r.start()
	alice := r.signIn(fakeoidc.AliceEmail)
	channel := alice.newChannel("uploads")

	create := func(name, mime, purpose string, size int64) contractAnswer {
		return postJSON(r, alice.http, "kiwi.files.v1.FileService/CreateUpload",
			fmt.Sprintf(`{"channelId":%q,"name":%q,"mime":%q,"size":"%d","purpose":%q}`, channel, name, mime, size, purpose))
	}
	complete := func(fileID string) contractAnswer {
		return postJSON(r, alice.http, "kiwi.files.v1.FileService/CompleteUpload", fmt.Sprintf(`{"fileId":%q}`, fileID))
	}
	// declared is an upload that CreateUpload accepted, with the URL its bytes go to.
	type declared struct{ fileID, url, mime string }
	declare := func(name, mime, purpose string, size int64) declared {
		t.Helper()
		got := create(name, mime, purpose, size)
		if got.HTTPStatus != http.StatusOK {
			t.Fatalf("CreateUpload(%s) = %d %s", name, got.HTTPStatus, got.Body)
		}
		var res struct {
			FileID    string `json:"fileId"`
			UploadURL string `json:"uploadUrl"`
		}
		if err := json.Unmarshal(got.Body, &res); err != nil {
			t.Fatal(err)
		}
		return declared{fileID: res.FileID, url: res.UploadURL, mime: mime}
	}
	key := func(d declared) string { return "uploads/" + r.ws.String() + "/" + d.fileID }
	put := func(url, contentType string, body []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Origin", r.URL) // the signed URLs are behind the origin check too
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PUT: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("PUT = %d", resp.StatusCode)
		}
	}
	// signAgain signs a PUT for d's object that allows what the one CreateUpload signed
	// does not (more bytes, or another type), which is how the store comes to hold an
	// object that CompleteUpload must find different from what was declared.
	signAgain := func(d declared, contentType string, max int64) string {
		t.Helper()
		url, _, err := r.app.blob.SignedPut(t.Context(), key(d), contentType, max, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return url
	}

	wavHeadless := append([]byte("ID3"), make([]byte, 100)...) // an MP3 labelled WAV

	cases := []struct {
		name   string
		reason string // the reason the person is told; "" for a refusal that names none
		answer func() contractAnswer
	}{
		{"create_empty", "empty", func() contractAnswer {
			return create("notes.txt", "text/plain", "UPLOAD_PURPOSE_ATTACHMENT", 0)
		}},
		{"create_too_large", "too_large", func() contractAnswer {
			return create("big.pdf", "application/pdf", "UPLOAD_PURPOSE_ATTACHMENT", 25<<20+1)
		}},
		{"create_unsupported_type", "unsupported_type", func() contractAnswer {
			return create("plan.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "UPLOAD_PURPOSE_ATTACHMENT", 100)
		}},
		{"create_unsupported_audio", "unsupported_audio", func() contractAnswer {
			return create("standup.mp3", "audio/mpeg", "UPLOAD_PURPOSE_RECORDING", 100)
		}},
		// A refusal that has no reason of its own (no name to store the file under) is the sentinel's text only.
		{"create_no_reason", "", func() contractAnswer {
			return create("", "text/plain", "UPLOAD_PURPOSE_ATTACHMENT", 100)
		}},
		{"complete_empty", "empty", func() contractAnswer {
			d := declare("empty.txt", "text/plain", "UPLOAD_PURPOSE_ATTACHMENT", 10)
			put(d.url, d.mime, nil)
			return complete(d.fileID)
		}},
		{"complete_too_large", "too_large", func() contractAnswer {
			d := declare("grew.txt", "text/plain", "UPLOAD_PURPOSE_ATTACHMENT", 10)
			put(signAgain(d, d.mime, 1000), d.mime, bytes.Repeat([]byte("x"), 100))
			return complete(d.fileID)
		}},
		{"complete_unsupported_type", "unsupported_type", func() contractAnswer {
			d := declare("notes.pdf", "application/pdf", "UPLOAD_PURPOSE_ATTACHMENT", 10)
			put(signAgain(d, "text/plain", 10), "text/plain", []byte("hello"))
			return complete(d.fileID)
		}},
		{"complete_unsupported_audio", "unsupported_audio", func() contractAnswer {
			d := declare("standup.wav", "audio/wav", "UPLOAD_PURPOSE_RECORDING", int64(len(wavHeadless)))
			put(d.url, d.mime, wavHeadless)
			return complete(d.fileID)
		}},
		{"complete_size_mismatch", "size_mismatch", func() contractAnswer {
			d := declare("changes.txt", "text/plain", "UPLOAD_PURPOSE_ATTACHMENT", 100)
			put(d.url, d.mime, []byte("0123456789"))
			shrink.arm(key(d))
			return complete(d.fileID)
		}},
	}

	names := make([]string, len(cases))
	for i, tc := range cases {
		names[i] = tc.name
		t.Run(tc.name, func(t *testing.T) {
			got := tc.answer()
			if got.HTTPStatus != http.StatusBadRequest {
				t.Fatalf("answered %d %s, want 400 invalid_argument", got.HTTPStatus, got.Body)
			}
			refused := got.refusal(t)
			if refused.Code != "invalid_argument" {
				t.Fatalf("code = %q, want invalid_argument (%s)", refused.Code, got.Body)
			}
			if tc.reason == "" {
				if refused.Message != "invalid" {
					t.Errorf("message = %q, want the sentinel's text only", refused.Message)
				}
			} else if !regexp.MustCompile(`^docs: upload rejected \(` + tc.reason + `\): [A-Za-z0-9 -]+$`).MatchString(refused.Message) {
				t.Errorf("message = %q, want it to name the reason %q and say it in one fixed sentence", refused.Message, tc.reason)
			}
			checkFixtureIn(t, uploadContractDir, tc.name, got)
		})
	}

	t.Run("every fixture has a case here", func(t *testing.T) { noStrayFixtures(t, uploadContractDir, names) })
}
