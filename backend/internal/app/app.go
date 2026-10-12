// Package app is the composition root: it builds the ports (Postgres, Valkey, the bus,
// the blob store, the model, Google) and the domain services on them, and runs every
// role this process has been given (KIWI_ROLES) behind one HTTP port. The domains keep
// their package boundaries; this package is the only one that knows all of them.
//
//   - api: the Connect handlers of every domain, the sign-in routes and the signed blob
//     URLs, at /api/.
//   - realtime: the WebSocket gateway, at /ws.
//   - worker: the consumers doc.analyze, meeting.ingest, meeting.transcribe,
//     meeting.transcript_check, embed.index and task.draft, the outbox relay and the
//     Meet poll.
//   - agents: the consumer agent.run.
//
// /healthz and /readyz are served by the listener itself. The process is ready when
// everything is registered and for as long as Postgres and Valkey answer.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"kiwi/internal/agents"
	"kiwi/internal/api"
	"kiwi/internal/chat"
	"kiwi/internal/docs"
	"kiwi/internal/identity"
	"kiwi/internal/knowledge"
	"kiwi/internal/meetings"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/blob"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/google"
	"kiwi/internal/platform/httpx"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/llm/llmfactory"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/realtime"
	"kiwi/internal/tasks"
)

const (
	roleAPI      = config.RoleAPI
	roleRealtime = config.RoleRealtime
	roleWorker   = config.RoleWorker
	roleAgents   = config.RoleAgents

	// listenAddr is the one port of the process: /api/, /ws, /healthz and /readyz.
	listenAddr = ":8080"

	// maxAttempts is how many times the bus delivers a message to a consumer before it
	// gives up. docs and meetings settle a failure on their fifth delivery (their own
	// finalAttempt constants) and agents on Deps.FinalAttempt, which is this number: the
	// bus must dead-letter exactly where they stop retrying, or a file, a meeting or a
	// run is left in progress for good.
	maxAttempts = 5

	// relayConcurrency is how many outbox rows are in flight. Each held row pins one
	// connection of the system pool (16) and an open transaction for as long as its
	// handlers work, and the sign-in callback and the Meet poll need that pool as well,
	// so this stays well below it. A meeting that is being transcribed or an agent run
	// holds its row for minutes, and four of them at once is more than a laptop runs.
	relayConcurrency = 4

	// defaultPollEvery is how often Meet is asked about the meetings that wait for a
	// transcript.
	defaultPollEvery = 30 * time.Second

	// googleTimeout bounds each request to the Meet API.
	googleTimeout = 30 * time.Second

	// gatewayGrace is how long the WebSocket connections get to close when the process stops.
	gatewayGrace = 10 * time.Second

	envProd = "prod"
	// devOrigin is the Vite dev server of the web app, which is allowed to open the
	// WebSocket outside prod.
	devOrigin = "localhost:5173"
)

// Bus is what the app needs of the message bus: to publish, and to register a consumer
// without blocking, so that every consumer can be registered before the relay starts.
// *bus.Memory is the only implementation that is built.
type Bus interface {
	bus.Publisher
	Register(consumer string, topics []string, h bus.Handler)
}

type options struct {
	llm       llm.Client
	google    google.Client
	bus       Bus
	listener  net.Listener
	drain     *time.Duration
	pollEvery time.Duration
	wrapBlob  func(blob.Store) blob.Store
}

// Option replaces something New would build, for the caller that has it already (serve
// hands over the model it checked) and for tests.
type Option func(*options)

// WithLLM uses c as the model instead of the one KIWI_LLM_PROVIDER selects.
func WithLLM(c llm.Client) Option { return func(o *options) { o.llm = c } }

// WithGoogle uses g for Meet instead of the REST client over each user's own grant.
func WithGoogle(g google.Client) Option { return func(o *options) { o.google = g } }

// WithBus uses b instead of a new in-process bus.
func WithBus(b Bus) Option { return func(o *options) { o.bus = b } }

// WithListener makes Run serve on ln instead of listening on :8080.
func WithListener(ln net.Listener) Option { return func(o *options) { o.listener = ln } }

// WithDrainTimeout sets how long Run keeps serving, not ready, after its context ends,
// before it stops the server; the default is httpx's.
func WithDrainTimeout(d time.Duration) Option { return func(o *options) { o.drain = &d } }

// WithPollInterval sets how often Meet is polled; the default is 30 s.
func WithPollInterval(d time.Duration) Option { return func(o *options) { o.pollEvery = d } }

// WithBlobWrap has the domains use wrap(store) as their object store, where store is
// the one KIWI_BLOB_URL selects; the signed URLs are still served by store itself. It
// is a seam for tests that need the store to misbehave.
func WithBlobWrap(wrap func(blob.Store) blob.Store) Option {
	return func(o *options) { o.wrapBlob = wrap }
}

// App is the process: its ports, its services and the roles it runs.
type App struct {
	cfg config.Config
	opt options

	db     *db.DB
	cache  *cache.Cache
	blob   blob.Store
	llm    llm.Client
	google google.Client
	bus    Bus

	gateway  *realtime.Gateway // nil without the realtime role
	handlers []api.Handler
	api      http.Handler // nil without the api role
	jobs     jobs

	closeOnce sync.Once
	closers   []func()
}

func (a *App) has(r config.Role) bool { return slices.Contains(a.cfg.Roles, r) }

// New opens the ports and builds the services of the roles in cfg.Roles. It dials
// Valkey and the sign-in provider, so it fails at once, naming the variable, when they
// cannot be reached; Postgres is dialled lazily, and Run is not ready until it answers.
// The caller closes the App, whether or not Run was called.
func New(ctx context.Context, cfg config.Config, opts ...Option) (_ *App, err error) {
	a := &App{cfg: cfg, opt: options{pollEvery: defaultPollEvery}}
	for _, opt := range opts {
		opt(&a.opt)
	}
	defer func() {
		if err != nil {
			a.Close()
		}
	}()

	if a.db, err = db.Open(ctx, cfg); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	a.closers = append(a.closers, a.db.Close)
	if a.cache, err = cache.Open(ctx, cfg.ValkeyURL, cache.Options{}); err != nil {
		return nil, fmt.Errorf("app: KIWI_VALKEY_URL: %w", err)
	}
	a.closers = append(a.closers, func() { _ = a.cache.Close() })

	if a.has(roleRealtime) {
		// The gateway drains in httpx: Run keeps serving, not ready, before the
		// connections are closed.
		a.gateway = realtime.New(
			realtime.Deps{Cache: a.cache, Membership: chat.NewMembership(a.db, a.cache)},
			realtime.Options{OriginPatterns: originPatterns(cfg)},
		)
	}
	if a.has(roleAPI) || a.has(roleWorker) || a.has(roleAgents) {
		if err := a.wire(ctx); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// wire builds everything the api, worker and agents roles share.
func (a *App) wire(ctx context.Context) error {
	cfg := a.cfg
	var err error

	a.bus = a.opt.bus
	if a.bus == nil {
		a.bus = newMemoryBus()
	}
	a.llm = a.opt.llm
	if a.llm == nil {
		if a.llm, err = llmfactory.New(cfg); err != nil {
			return fmt.Errorf("app: llm: %w", err)
		}
	}

	ident, err := identity.New(ctx, identity.Deps{Config: cfg, DB: a.db, Cache: a.cache})
	if err != nil {
		return fmt.Errorf("app: sign-in provider (KIWI_OIDC_ISSUER, KIWI_OIDC_INTERNAL_URL): %w", err)
	}

	if a.blob, err = blob.Open(ctx, cfg); err != nil {
		return fmt.Errorf("app: %w", err)
	}
	if closer, ok := a.blob.(io.Closer); ok {
		a.closers = append(a.closers, func() { _ = closer.Close() })
	}
	blobHandler, ok := a.blob.(http.Handler)
	if !ok {
		return fmt.Errorf("app: the blob store (%T) does not serve signed URLs", a.blob)
	}
	if a.opt.wrapBlob != nil {
		a.blob = a.opt.wrapBlob(a.blob)
	}

	a.google = a.opt.google
	if a.google == nil {
		if a.google, err = google.New(ident.UserTokenSource, google.Options{HTTPClient: &http.Client{Timeout: googleTimeout}}); err != nil {
			return fmt.Errorf("app: %w", err)
		}
	}

	registry, err := agents.LoadRegistry()
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	knownAgent := func(name string) bool { _, ok := registry[name]; return ok }

	// Membership is read through Valkey (members:{channel}, a 10 minute backstop TTL; the
	// invalidation counter members:{channel}:epoch is what keeps a stale fill out), so
	// Valkey must not evict keys under memory pressure: the local one sets no maxmemory.
	az := authz.New(chat.NewMembership(a.db, a.cache))
	chatSvc := chat.New(chat.Deps{DB: a.db, Authz: az, Cache: a.cache, KnownAgent: knownAgent})
	docDeps := docs.Deps{DB: a.db, Authz: az, Blob: a.blob, Chat: chatSvc, LLM: a.llm}
	docSvc := docs.New(docDeps)
	meetingDeps := meetings.Deps{
		DB: a.db, Authz: az, Google: a.google, Chat: chatSvc, Summarizer: meetings.NewSummarizer(a.llm),
		Blob: a.blob, LLM: a.llm, FileChecker: fileChecker(docSvc),
	}
	meetingSvc := meetings.New(meetingDeps)
	taskDeps := tasks.Deps{DB: a.db, Authz: az, MemberChannels: chatSvc.MemberChannelIDs, ChannelMembers: chatSvc.ListMembers}
	taskSvc := tasks.New(taskDeps)
	knowledgeDeps := knowledge.Deps{DB: a.db, LLM: a.llm, Authz: az, Chat: chatSvc, Meetings: meetingSvc, Docs: docSvc}
	agentDeps := agents.Deps{
		DB: a.db, Authz: az, Chat: chatSvc, Knowledge: knowledge.New(knowledgeDeps), Tasks: taskSvc, LLM: a.llm,
		Frames: a.cache, FinalAttempt: maxAttempts,
	}
	agentSvc := agents.New(agentDeps)

	a.jobs = jobs{worker: workerJobs(docDeps, meetingDeps, knowledgeDeps, taskDeps), agents: agentJobs(agentDeps)}

	// api.New gives every Build the error mapping, so a domain's errors reach clients as
	// Connect codes and nothing of their cause.
	a.handlers = []api.Handler{
		{Build: func(opts ...connect.HandlerOption) (string, http.Handler) {
			return identity.Handler(ident, api.ActorFrom, opts...)
		}},
		{Build: func(opts ...connect.HandlerOption) (string, http.Handler) {
			return chat.Handler(chatSvc, api.ActorFrom, opts...)
		}},
		{Build: func(opts ...connect.HandlerOption) (string, http.Handler) {
			return docs.Handler(docSvc, api.ActorFrom, opts...)
		}},
		{Build: func(opts ...connect.HandlerOption) (string, http.Handler) {
			return meetings.Handler(meetingSvc, api.ActorFrom, opts...)
		}},
		{Build: func(opts ...connect.HandlerOption) (string, http.Handler) {
			return tasks.Handler(taskSvc, api.ActorFrom, opts...)
		}},
		{Build: func(opts ...connect.HandlerOption) (string, http.Handler) {
			return agents.Handler(agentSvc, api.ActorFrom, opts...)
		}},
	}
	if a.has(roleAPI) {
		a.api = api.New(api.Deps{
			PublicURL: cfg.PublicURL,
			Sessions:  a.cache,
			Auth:      ident.AuthHandler(),
			Blob:      blobHandler,
			Handlers:  a.handlers,
		})
	}
	return nil
}

// fileChecker answers the meetings domain, which makes a meeting from a recording and
// has to know the file is one, from the docs domain that holds it. filesv1.File carries
// neither the workspace nor the purpose, so they are derived: GetFile reads the file in
// the actor's workspace only (another workspace's file is not found), and the purpose
// follows from the type docs accepted the upload with, by the rule it applied.
func fileChecker(d docs.Service) meetings.FileChecker {
	return func(ctx context.Context, a authz.Actor, fileID uuid.UUID) (meetings.FileInfo, error) {
		f, err := d.GetFile(ctx, a, fileID)
		if err != nil {
			return meetings.FileInfo{}, err
		}
		channel, err := uuid.Parse(f.GetChannelId())
		if err != nil {
			return meetings.FileInfo{}, fmt.Errorf("app: file %s has the channel id %q: %w", fileID, f.GetChannelId(), errs.ErrInvalid)
		}
		return meetings.FileInfo{
			WorkspaceID: a.WorkspaceID,
			ChannelID:   channel,
			Purpose:     docs.PurposeOf(f.GetMime()),
			Status:      f.GetStatus(),
		}, nil
	}
}

// originPatterns are the origins besides the request's own host that may open the
// WebSocket: the host of the public URL, which is the page's origin behind a proxy that
// rewrites the Host header, and, outside prod, the Vite dev server.
func originPatterns(cfg config.Config) []string {
	var patterns []string
	if u, err := url.Parse(cfg.PublicURL); err == nil && u.Host != "" {
		patterns = append(patterns, u.Host)
	}
	if cfg.Env != envProd {
		patterns = append(patterns, devOrigin)
	}
	return patterns
}

// newMemoryBus is the in-process bus with the settings the domains rely on: maxAttempts
// deliveries and a dead letter logged at ERROR. With no backoff the bus waits 1 s, 5 s,
// 30 s and 2 min between attempts; a test passes a shorter one.
func newMemoryBus(backoff ...time.Duration) *bus.Memory {
	return bus.NewMemory(bus.MemoryOptions{MaxAttempts: maxAttempts, Backoff: backoff, OnDead: logDead})
}

func logDead(consumer string, m bus.Message, err error) {
	slog.Error("bus: message dead-lettered",
		"consumer", consumer, "topic", m.Topic, "message_id", m.ID, "workspace_id", m.Attrs["workspace_id"],
		"attempts", m.Attempt, "error", err)
}

// ping is the readiness check: the database pools and Valkey answer.
func (a *App) ping(ctx context.Context) error {
	if err := a.db.Ping(ctx); err != nil {
		return err
	}
	return a.cache.Ping(ctx)
}

// Run serves the roles of the process until ctx ends, and returns nil after a clean
// shutdown. The listener starts first, so the process is live (and answers 503 on
// /readyz) while it registers its consumers; the relay and the Meet poll start only
// after every consumer of every role is registered, and readiness only after that.
//
// On shutdown readiness fails first and the server keeps serving for the drain, then
// it stops; the WebSocket connections are closed after it; only then is the context of
// the relay and the jobs cancelled, which rolls back the rows in flight so that the
// next process delivers them again. Run may be called once.
func (a *App) Run(ctx context.Context) error {
	ln := a.opt.listener
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", listenAddr); err != nil {
			return fmt.Errorf("app: %w", err)
		}
	}
	srv := httpx.New(listenAddr, a.mux())
	if a.opt.drain != nil {
		srv.DrainTimeout = *a.opt.drain
	}
	srv.ReadyCheck = a.ping
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx, ln) }()

	// The jobs outlive the signal that stops the server: they are cancelled below,
	// after the drain.
	background, cancelBackground := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	defer func() {
		cancelBackground()
		wg.Wait()
	}()

	a.register()
	if a.has(roleWorker) {
		relay := outbox.NewRelay(a.db, a.bus, a.cache, Router{}, outbox.RelayOptions{Concurrency: relayConcurrency})
		wg.Go(func() { _ = relay.Run(background) })
		wg.Go(func() { a.pollMeet(background) })
	}
	srv.SetReady(true)
	if ctx.Err() != nil {
		srv.SetReady(false) // stopped while starting: Serve may have flipped it already
	} else {
		slog.Info("app: ready", "roles", a.cfg.Roles, "addr", ln.Addr().String())
	}

	err := <-served
	if a.gateway != nil {
		closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayGrace)
		a.gateway.Shutdown(closing)
		cancel()
	}
	return err
}

// mux routes the one port: the api role's handler under /api/ and the gateway at /ws.
// The probes are the listener's.
func (a *App) mux() http.Handler {
	mux := http.NewServeMux()
	if a.api != nil {
		mux.Handle("/api/", a.api)
	}
	if a.gateway != nil {
		mux.Handle("/ws", a.gateway)
	}
	return mux
}

// Close releases the ports. It is safe to call twice.
func (a *App) Close() {
	a.closeOnce.Do(func() {
		for _, c := range slices.Backward(a.closers) {
			c()
		}
	})
}
