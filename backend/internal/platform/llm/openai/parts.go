package openai

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"strings"

	"kiwi/internal/platform/llm"
)

// contentPart is one element of a user message.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// contentParts maps request parts to message content: text as text, an image as a
// data URL. What the local models cannot read is llm.ErrUnsupportedByProvider, and
// no request is made: a PDF with no text layer (the text layer itself comes as a text
// part), any other binary, and a part that is only a URI, which this provider has no
// way to fetch.
func contentParts(parts []llm.Part) ([]contentPart, error) {
	out := make([]contentPart, 0, len(parts))
	for i, p := range parts {
		switch {
		case p.Text != "":
			out = append(out, contentPart{Type: "text", Text: p.Text})
		case p.URI != "":
			return nil, fmt.Errorf("%w: part %d is a URI (%s) and this provider cannot read one", llm.ErrUnsupportedByProvider, i, mediaType(p.MIME))
		case len(p.Data) > 0:
			mt := mediaType(p.MIME)
			switch {
			case strings.HasPrefix(mt, "image/"):
				out = append(out, contentPart{Type: "image_url", ImageURL: &imageURL{URL: "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(p.Data)}})
			case mt == "application/pdf":
				return nil, fmt.Errorf("%w: part %d is a PDF without its text layer, and the local models cannot read a scan", llm.ErrUnsupportedByProvider, i)
			default:
				return nil, fmt.Errorf("%w: part %d is %q data", llm.ErrUnsupportedByProvider, i, mt)
			}
		default:
			return nil, errors.New("openai: a part has no text, no data and no URI")
		}
	}
	return out, nil
}

// mediaType is the MIME type of a part without its parameters, in lower case.
func mediaType(m string) string {
	mt, _, err := mime.ParseMediaType(m)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.SplitN(m, ";", 2)[0]))
	}
	return mt
}
