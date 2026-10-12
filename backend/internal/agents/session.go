package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/adk/v2/platform"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"kiwi/internal/agents/internal/store"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
)

const (
	// appName is the ADK application every session of the agents belongs to.
	appName = "kiwi"
	// askSession is the channel part of the session id of the Ask panel, which has no
	// channel of its own.
	askSession = "ask"
	// maxHistoryEvents is how many of its newest events a session hands the model. A
	// conversation lives as long as a person keeps asking in a channel, and every
	// event of it would otherwise ride along in every prompt for ever.
	maxHistoryEvents = 40
	// maxLoadedEvents bounds what a caller may ask for with GetRequest.NumRecentEvents.
	maxLoadedEvents = 1000

	pgForeignKeyViolation = "23503"
)

// Session is the conversation one run continues, and what the run may use: the
// events of the person's earlier turns with this agent in this channel (or the Ask
// panel), and the tools of this run only. A Runtime reads both and nothing else of
// the world.
type Session struct {
	// ID and UserID name the conversation in Service: one per (agent, user, channel
	// or "ask"). UserID is the person's UUID.
	ID, UserID string
	// Service stores the events of the conversation.
	Service session.Service
	// Tools are the tools of this run by name, with the run's collector and trigger
	// behind them. The spec decides which of them the agent is given.
	Tools map[string]tool.Tool
}

// sessionID is the id of the conversation of user with agent in channel, or in the
// Ask panel when channel is nil. The id is the primary key of agents.sessions by
// itself, so it carries the user, whom no two workspaces share.
func sessionID(agent string, user uuid.UUID, channel *uuid.UUID) string {
	where := askSession
	if channel != nil {
		where = channel.String()
	}
	return agent + ":" + user.String() + ":" + where
}

// sessionService is the ADK session.Service over agents.sessions and
// agents.session_events, for the one workspace it was made for. Every statement runs
// in db.InWorkspace, so row-level security keeps it inside that workspace.
//
// State is kept per session: the schema has no tables for state shared by an app or a
// user, so a key that says "app:" or "user:" is stored with the session like any other.
type sessionService struct {
	db *db.DB
	ws uuid.UUID
}

var _ session.Service = (*sessionService)(nil)

func newSessionService(d *db.DB, ws uuid.UUID) session.Service {
	return &sessionService{db: d, ws: ws}
}

// userID reads the person a session is for: sessions belong to users of the
// workspace, which agents.sessions.user_id is a foreign key to.
func userID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("agents: session user %q is not a user id: %w", raw, errs.ErrInvalid)
	}
	return id, nil
}

// Create implements session.Service. Creating a session that is already there for
// the same app and user returns it: two runs of one conversation may race to start
// it, and neither should fail for losing.
func (s *sessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if req.AppName == "" || req.UserID == "" {
		return nil, fmt.Errorf("agents: app_name and user_id are required, got app_name: %q, user_id: %q: %w", req.AppName, req.UserID, errs.ErrInvalid)
	}
	user, err := userID(req.UserID)
	if err != nil {
		return nil, err
	}
	id := req.SessionID
	if id == "" {
		id = platform.NewUUID(ctx)
	}
	state := make(map[string]any, len(req.State))
	for k, v := range req.State {
		if !strings.HasPrefix(k, session.KeyPrefixTemp) {
			state[k] = v
		}
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("agents: encode session state: %w", err)
	}

	var sess *pgSession
	err = s.db.InWorkspace(ctx, s.ws, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.InsertSession(ctx, store.InsertSessionParams{
			ID: id, WorkspaceID: s.ws, AppName: req.AppName, UserID: user, State: stateJSON,
		}); err != nil {
			return err
		}
		row, err := q.GetSession(ctx, store.GetSessionParams{ID: id, AppName: req.AppName, UserID: user})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("agents: session %q already exists for another user or app: %w", id, errs.ErrConflict)
		}
		if err != nil {
			return err
		}
		if sess, err = s.load(ctx, q, row, maxHistoryEvents, true, time.Time{}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation {
			return nil, fmt.Errorf("agents: session user %s is not a user of the workspace: %w", user, errs.ErrNotFound)
		}
		return nil, fmt.Errorf("agents: create session: %w", err)
	}
	return &session.CreateResponse{Session: sess}, nil
}

// Get implements session.Service. It loads the newest events of the session, from
// the start of the oldest turn they hold: a window that began with a tool's response
// would hand the model an answer to a call it never made, which providers refuse.
// A request that names NumRecentEvents asks for exactly that many and gets them.
func (s *sessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return nil, fmt.Errorf("agents: app_name, user_id, session_id are required, got app_name: %q, user_id: %q, session_id: %q: %w", req.AppName, req.UserID, req.SessionID, errs.ErrInvalid)
	}
	user, err := userID(req.UserID)
	if err != nil {
		return nil, err
	}
	limit, whole := maxHistoryEvents, true
	if req.NumRecentEvents > 0 {
		limit, whole = min(req.NumRecentEvents, maxLoadedEvents), false
	}

	var sess *pgSession
	err = s.db.InWorkspace(ctx, s.ws, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.GetSession(ctx, store.GetSessionParams{ID: req.SessionID, AppName: req.AppName, UserID: user})
		if err != nil {
			return err
		}
		sess, err = s.load(ctx, q, row, limit, whole, req.After)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", session.ErrNotFound, req.SessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("agents: get session: %w", err)
	}
	return &session.GetResponse{Session: sess}, nil
}

// load reads the newest limit events of row's session, oldest first. When wholeTurns
// is set the first event is a user's: leading events of a turn whose start is out of
// the window are dropped. Events before after are dropped too.
func (s *sessionService) load(ctx context.Context, q *store.Queries, row store.AgentsSession, limit int, wholeTurns bool, after time.Time) (*pgSession, error) {
	var rows []store.ListRecentSessionEventsRow
	if limit > 0 {
		var err error
		if rows, err = q.ListRecentSessionEvents(ctx, store.ListRecentSessionEventsParams{SessionID: row.ID, MaxEvents: int32(limit)}); err != nil {
			return nil, err
		}
	}
	evs := make([]*session.Event, 0, len(rows))
	for _, r := range rows {
		var ev session.Event
		if err := json.Unmarshal(r.Event, &ev); err != nil {
			return nil, fmt.Errorf("decode event %d of session %s: %w", r.ID, row.ID, err)
		}
		evs = append(evs, &ev)
	}
	if wholeTurns {
		i := slices.IndexFunc(evs, func(ev *session.Event) bool { return ev.Author == "user" })
		if i < 0 {
			i = len(evs)
		}
		evs = evs[i:]
	}
	if !after.IsZero() {
		evs = slices.DeleteFunc(evs, func(ev *session.Event) bool { return ev.Timestamp.Before(after) })
	}
	state := map[string]any{}
	if len(row.State) > 0 {
		if err := json.Unmarshal(row.State, &state); err != nil {
			return nil, fmt.Errorf("decode state of session %s: %w", row.ID, err)
		}
	}
	return &pgSession{id: row.ID, app: row.AppName, user: row.UserID.String(), state: state, events: evs, updated: row.UpdatedAt.Time}, nil
}

// List implements session.Service. The sessions come without their events.
func (s *sessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if req.AppName == "" {
		return nil, fmt.Errorf("agents: app_name is required: %w", errs.ErrInvalid)
	}
	var user *string
	if req.UserID != "" {
		id, err := userID(req.UserID)
		if err != nil {
			return nil, err
		}
		user = new(id.String())
	}
	out := make([]session.Session, 0)
	err := s.db.InWorkspace(ctx, s.ws, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := store.New(tx).ListSessions(ctx, store.ListSessionsParams{AppName: req.AppName, UserID: user})
		if err != nil {
			return err
		}
		for _, row := range rows {
			sess, err := s.load(ctx, store.New(tx), row, 0, false, time.Time{})
			if err != nil {
				return err
			}
			out = append(out, sess)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agents: list sessions: %w", err)
	}
	return &session.ListResponse{Sessions: out}, nil
}

// Delete implements session.Service. A session that is not there is not an error.
func (s *sessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if req.AppName == "" || req.UserID == "" || req.SessionID == "" {
		return fmt.Errorf("agents: app_name, user_id, session_id are required, got app_name: %q, user_id: %q, session_id: %q: %w", req.AppName, req.UserID, req.SessionID, errs.ErrInvalid)
	}
	user, err := userID(req.UserID)
	if err != nil {
		return err
	}
	err = s.db.InWorkspace(ctx, s.ws, func(ctx context.Context, tx pgx.Tx) error {
		return store.New(tx).DeleteSession(ctx, store.DeleteSessionParams{ID: req.SessionID, AppName: req.AppName, UserID: user})
	})
	if err != nil {
		return fmt.Errorf("agents: delete session: %w", err)
	}
	return nil
}

// AppendEvent implements session.Service. A partial event is a piece of text on its
// way to the screen and is not stored; an event without an id is given one, in place;
// "temp:" keys of its state delta are dropped before it is written.
func (s *sessionService) AppendEvent(ctx context.Context, cur session.Session, ev *session.Event) error {
	if cur == nil {
		return errors.New("agents: session is nil")
	}
	if ev == nil {
		return errors.New("agents: event is nil")
	}
	sess, ok := cur.(*pgSession)
	if !ok {
		return fmt.Errorf("agents: unexpected session type %T for session %s", cur, cur.ID())
	}
	if ev.Partial {
		return nil
	}
	if ev.ID == "" {
		ev.ID = platform.NewUUID(ctx)
	}
	kept := withoutTempState(ev)
	raw, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("agents: encode event %s: %w", ev.ID, err)
	}
	delta := kept.Actions.StateDelta
	if delta == nil {
		delta = map[string]any{}
	}
	deltaJSON, err := json.Marshal(delta)
	if err != nil {
		return fmt.Errorf("agents: encode state delta of event %s: %w", ev.ID, err)
	}
	at := ev.Timestamp
	if at.IsZero() {
		at = platform.Now(ctx)
	}

	err = s.db.InWorkspace(ctx, s.ws, func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.TouchSession(ctx, store.TouchSessionParams{StateDelta: deltaJSON, UpdatedAt: pgtype.Timestamptz{Time: at, Valid: true}, ID: sess.id})
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %q, cannot apply event", session.ErrNotFound, sess.id)
		}
		return q.InsertSessionEvent(ctx, store.InsertSessionEventParams{SessionID: sess.id, WorkspaceID: s.ws, Event: raw})
	})
	if errors.Is(err, session.ErrNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("agents: append event %s: %w", ev.ID, err)
	}
	sess.apply(kept, at)
	return nil
}

// withoutTempState is ev without the "temp:" keys of its state delta; ev itself is
// left alone when there are none.
func withoutTempState(ev *session.Event) *session.Event {
	hasTemp := false
	for k := range ev.Actions.StateDelta {
		if strings.HasPrefix(k, session.KeyPrefixTemp) {
			hasTemp = true
			break
		}
	}
	if !hasTemp {
		return ev
	}
	cp := *ev
	cp.Actions.StateDelta = make(map[string]any, len(ev.Actions.StateDelta))
	for k, v := range ev.Actions.StateDelta {
		if !strings.HasPrefix(k, session.KeyPrefixTemp) {
			cp.Actions.StateDelta[k] = v
		}
	}
	return &cp
}

// pgSession is a session loaded by sessionService. The runner reads its events while
// it runs, and AppendEvent keeps them current, so every access holds mu.
type pgSession struct {
	id, app, user string

	mu      sync.RWMutex
	state   map[string]any
	events  []*session.Event
	updated time.Time
}

var _ session.Session = (*pgSession)(nil)

func (s *pgSession) ID() string      { return s.id }
func (s *pgSession) AppName() string { return s.app }
func (s *pgSession) UserID() string  { return s.user }

func (s *pgSession) State() session.State { return &pgState{s} }

func (s *pgSession) Events() session.Events {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return eventList(slices.Clone(s.events))
}

func (s *pgSession) LastUpdateTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updated
}

// apply makes ev, which was stored, part of the live session.
func (s *pgSession) apply(ev *session.Event, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	maps.Copy(s.state, ev.Actions.StateDelta)
	s.updated = at
}

// pgState is the state of a session. Set changes the live session; what survives it
// is what an event's state delta wrote.
type pgState struct{ s *pgSession }

func (st *pgState) Get(key string) (any, error) {
	st.s.mu.RLock()
	defer st.s.mu.RUnlock()
	v, ok := st.s.state[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}

func (st *pgState) Set(key string, value any) error {
	st.s.mu.Lock()
	defer st.s.mu.Unlock()
	st.s.state[key] = value
	return nil
}

func (st *pgState) All() iter.Seq2[string, any] {
	st.s.mu.RLock()
	snapshot := maps.Clone(st.s.state)
	st.s.mu.RUnlock()
	return func(yield func(string, any) bool) {
		for k, v := range snapshot {
			if !yield(k, v) {
				return
			}
		}
	}
}

// eventList is the events of a session as session.Events.
type eventList []*session.Event

func (e eventList) All() iter.Seq[*session.Event] { return slices.Values(e) }
func (e eventList) Len() int                      { return len(e) }
func (e eventList) At(i int) *session.Event {
	if i < 0 || i >= len(e) {
		return nil
	}
	return e[i]
}
