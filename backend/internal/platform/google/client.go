package google

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	meet "google.golang.org/api/meet/v2"
	"google.golang.org/api/option"
)

// meetScope is the one scope every Meet call asks for: it covers the spaces this
// application creates and their conference records, transcripts and participants.
const meetScope = meet.MeetingsSpaceCreatedScope

// TokenFunc returns a token source for userID's own Google grant covering scope. It
// is wired to identity.Service.UserTokenSource; a *ConsentRequiredError it returns
// reaches the caller of the Client unchanged.
type TokenFunc func(ctx context.Context, userID uuid.UUID, scope string) (oauth2.TokenSource, error)

// Options tune the Meet client; the zero value talks to Google.
type Options struct {
	// MeetBaseURL replaces the Meet REST endpoint (https://meet.googleapis.com/);
	// tests point it at an httptest server.
	MeetBaseURL string
	// HTTPClient supplies the transport and timeout of every request; the user's
	// token is added on top of its transport. Nil uses http.DefaultTransport.
	HTTPClient *http.Client
}

type client struct {
	tokens   TokenFunc
	endpoint string
	base     http.RoundTripper
	timeout  time.Duration
}

var _ Client = (*client)(nil)

// New returns the Meet REST client. Each call fetches a token source for the user it
// acts for, so the request carries that user's own grant and nothing else.
func New(tokens TokenFunc, opt Options) (Client, error) {
	if tokens == nil {
		return nil, errors.New("google: New needs a TokenFunc")
	}
	c := &client{tokens: tokens, base: http.DefaultTransport}
	if opt.HTTPClient != nil {
		c.timeout = opt.HTTPClient.Timeout
		if opt.HTTPClient.Transport != nil {
			c.base = opt.HTTPClient.Transport
		}
	}
	if opt.MeetBaseURL != "" {
		// The generated client resolves request paths against this base, so it
		// must end in a slash to keep any path prefix.
		c.endpoint = strings.TrimSuffix(opt.MeetBaseURL, "/") + "/"
	}
	return c, nil
}

// service builds a Meet service whose requests carry userID's token.
func (c *client) service(ctx context.Context, userID uuid.UUID) (*meet.Service, error) {
	src, err := c.tokens(ctx, userID, meetScope)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	hc := &http.Client{Transport: &oauth2.Transport{Source: src, Base: c.base}, Timeout: c.timeout}
	opts := []option.ClientOption{option.WithHTTPClient(hc)}
	if c.endpoint != "" {
		opts = append(opts, option.WithEndpoint(c.endpoint))
	}
	svc, err := meet.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("build Meet service: %w", err)
	}
	return svc, nil
}

// wrap prefixes err with the package name. A *ConsentRequiredError anywhere in the
// chain (from the TokenFunc, or from a token source asked for a token mid-request)
// is returned itself so callers see it unchanged. A failure of Meet comes back with
// its *MeetError beside it (see classify); the rest stays a plain error.
func wrap(err error) error {
	var cr *ConsentRequiredError
	if errors.As(err, &cr) {
		return cr
	}
	err = fmt.Errorf("google: %w", err)
	if me := classify(err); me != nil {
		return &meetFailure{err: err, meet: me}
	}
	return err
}
