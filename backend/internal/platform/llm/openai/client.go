// Package openai is the llm.Client for OpenAI-compatible servers: LiteLLM for
// generation and transcription, Ollama for embeddings. The models behind LiteLLM
// reason before they answer, so the adapter pays for that reasoning in the output
// limit and never lets it out; requests carry no setting only Gemini understands.
package openai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/openaimodel"

	"kiwi/internal/platform/config"
	"kiwi/internal/platform/llm"
)

// maxErrorBody bounds how much of a failing answer is kept in an llm.HTTPError.
const maxErrorBody = 4 << 10

// maxAnswer bounds a successful non-streaming answer.
const maxAnswer = 32 << 20

type client struct {
	base       string // LiteLLM origin, without /v1 and without a trailing slash
	embedBase  string // Ollama origin
	key        string
	flash, pro string
	embedModel string
	asrModel   string
	hosts      []string // hosts of both endpoints, to keep out of errors
	hc         *http.Client

	// sleep waits d between attempts at a transcription chunk, or until ctx ends.
	// Tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

var _ llm.Client = (*client)(nil)

// New returns the client for the LiteLLM and Ollama endpoints in cfg. hc carries
// every request; nil means a client with no timeout of its own, so the context of
// each call is the only deadline.
func New(cfg config.Config, hc *http.Client) (llm.Client, error) {
	base, err := origin("KIWI_LLM_BASE_URL", cfg.LLMBaseURL)
	if err != nil {
		return nil, err
	}
	embedBase, err := origin("KIWI_EMBED_BASE_URL", cfg.EmbedBaseURL)
	if err != nil {
		return nil, err
	}
	for name, v := range map[string]string{
		"KIWI_LLM_API_KEY":     cfg.LLMAPIKey,
		"KIWI_LLM_MODEL_FLASH": cfg.LLMModelFlash,
		"KIWI_LLM_MODEL_PRO":   cfg.LLMModelPro,
		"KIWI_EMBED_MODEL":     cfg.EmbedModel,
	} {
		if v == "" {
			return nil, fmt.Errorf("openai: %s is not set", name)
		}
	}
	if hc == nil {
		hc = &http.Client{}
	}
	return &client{
		base: base, embedBase: embedBase, key: cfg.LLMAPIKey,
		flash: cfg.LLMModelFlash, pro: cfg.LLMModelPro,
		embedModel: cfg.EmbedModel, asrModel: cfg.ASRModel,
		hosts: hostsOf(base, embedBase),
		hc:    hc, sleep: sleepCtx,
	}, nil
}

// hostsOf lists the host and the host with its port of each origin, longest first, so
// that "host:port" is replaced before "host".
func hostsOf(origins ...string) []string {
	var hosts []string
	for _, o := range origins {
		u, _ := url.Parse(o) // checked by origin
		for _, h := range []string{u.Host, u.Hostname()} {
			if len(h) >= minScrubbed && !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	slices.SortFunc(hosts, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	return hosts
}

// origin checks that raw is an http(s) URL with a host and no credentials, and
// returns it without a trailing slash. The error names the variable, never the value:
// a URL may carry what must not be logged.
func origin(name, raw string) (string, error) {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("openai: %s must be an http(s) URL with a host, without credentials, query or fragment", name)
	}
	return strings.TrimRight(raw, "/"), nil
}

// EmbeddingModel implements llm.Client.
func (c *client) EmbeddingModel() string { return c.embedModel }

// ADKModel implements llm.Client. ADK speaks the Responses API unless told
// otherwise, and LiteLLM here serves Chat Completions; the key is passed
// explicitly because an empty one makes the SDK fall back to OPENAI_API_KEY from the
// environment and send it to this host. ADK rejects the settings only Gemini
// understands, so none can leak through it. Callers must give a MaxOutputTokens of at
// least about 1100, or a ThinkingConfig with ThinkingBudget 0: with thinking on, a
// smaller limit is spent on reasoning and ADK reports an answer without text. What its
// errors say is scrubbed like the adapter's own (see adkTransport).
func (c *client) ADKModel(tier llm.Tier) (model.LLM, error) {
	id, err := c.modelFor(tier)
	if err != nil {
		return nil, err
	}
	to, _ := url.Parse(c.base) // checked by origin
	hc := *c.hc
	hc.Transport = &adkTransport{base: cmp.Or(c.hc.Transport, http.DefaultTransport), to: to, scrub: c.scrub}
	return openaimodel.NewModel(context.Background(), id, &openaimodel.ClientConfig{
		APIKey:     c.key,
		BaseURL:    adkBase + "/v1/",
		API:        openaimodel.APIChatCompletions,
		HTTPClient: &hc,
	})
}

// modelFor maps a tier to its configured model id; no tier means flash.
func (c *client) modelFor(tier llm.Tier) (string, error) {
	switch tier {
	case llm.TierFlash, "":
		return c.flash, nil
	case llm.TierPro:
		return c.pro, nil
	}
	return "", fmt.Errorf("openai: unknown tier %q", tier)
}

// post sends in as JSON to url and returns the answer when its status is a success.
// Credentials go to LiteLLM only: bearer says whether this request is one.
func (c *client) post(ctx context.Context, endpoint string, bearer bool, in any) (*http.Response, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("openai: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("openai: build request")
	}
	req.Header.Set("Content-Type", "application/json")
	return c.send(req, bearer)
}

// send performs req and turns a failing status into an *llm.HTTPError. The hosts of the
// endpoints are removed from what is reported: the URL the HTTP client puts in its
// errors, the name in a DNS failure, and whatever a gateway repeats in its body. They
// are configuration that must not reach a log.
func (c *client) send(req *http.Request, bearer bool) (*http.Response, error) {
	if bearer {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, &requestError{msg: c.scrub(err.Error()), err: err}
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, &llm.HTTPError{Status: resp.StatusCode, Body: c.scrub(string(b))}
	}
	return resp, nil
}

// requestError is a failure to get an answer. Its text has the hosts removed; err stays
// reachable with errors.Is and errors.As.
type requestError struct {
	msg string
	err error
}

func (e *requestError) Error() string { return "openai: request failed: " + e.msg }
func (e *requestError) Unwrap() error { return e.err }

// scrub replaces the key and the hosts of the endpoints in s. A key shorter than
// minScrubbed is left alone: it would mangle every text it is found in.
func (c *client) scrub(s string) string {
	if len(c.key) >= minScrubbed {
		s = strings.ReplaceAll(s, c.key, "<key>")
	}
	for _, h := range c.hosts {
		s = strings.ReplaceAll(s, h, "<host>")
	}
	return s
}

// minScrubbed is the shortest key or host that scrub replaces.
const minScrubbed = 3

// adkBase is the origin the ADK's SDK is told it calls. The SDK writes the URL of a
// request into the errors it returns (net/http around a transport's error, the SDK
// around a failing status), after any transport has had its say, so the real origin
// would reach the log of a failed run. The SDK is given an origin that names no host,
// and adkTransport sends its requests to the real one.
const adkBase = "http://llm.invalid"

// adkTransport is the transport of the HTTP client of the ADK model: it moves a request
// the SDK addressed to adkBase to the origin the adapter is configured with, and scrubs
// what comes back the way the adapter scrubs its own errors: a failed round trip, and
// the body of a failing status, which a gateway may fill with its upstream's address or
// the key it was given.
type adkTransport struct {
	base  http.RoundTripper
	to    *url.URL // the real origin, with the path it is served under, without a trailing slash
	scrub func(string) string
}

// RoundTrip implements http.RoundTripper.
func (t *adkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host = t.to.Scheme, t.to.Host
	out.URL.Path, out.URL.RawPath = t.to.Path+req.URL.Path, ""
	out.Host = t.to.Host
	resp, err := t.base.RoundTrip(out)
	if err != nil {
		return nil, &requestError{msg: t.scrub(err.Error()), err: err}
	}
	if resp.StatusCode >= 300 {
		answer := resp.Body
		defer func() { _ = answer.Close() }()
		b, _ := io.ReadAll(io.LimitReader(answer, maxErrorBody))
		resp.Body = io.NopCloser(strings.NewReader(t.scrub(string(b))))
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
	}
	return resp, nil
}

// postJSON sends in and decodes the JSON answer into out.
func (c *client) postJSON(ctx context.Context, endpoint string, bearer bool, in, out any) error {
	resp, err := c.post(ctx, endpoint, bearer, in)
	if err != nil {
		return err
	}
	return decode(resp, out)
}

// decode reads the whole answer into out and closes it.
func decode(resp *http.Response, out any) error {
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAnswer)).Decode(out); err != nil {
		return fmt.Errorf("openai: read answer: %w", err)
	}
	return nil
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
