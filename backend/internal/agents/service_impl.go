package agents

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentsv1 "kiwi/gen/kiwi/agents/v1"
	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/agents/internal/store"
	"kiwi/internal/chat"
	"kiwi/internal/knowledge"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/tasks"
)

// maxQuestionRunes bounds a question: the one an Ask-panel request carries and the
// body of the message that mentions an agent, which chat accepts up to 8000 runes and
// which is cut to this before it is stored as the run's question and given to the model.
const maxQuestionRunes = 4000

const (
	// defaultRunTimeout is how long one attempt of a run may take: the model client has no
	// timeout of its own, and a model that runs away would hold the run until the end of
	// the context.
	defaultRunTimeout = 120 * time.Second
	// defaultFinalAttempt is the delivery the bus dead-letters a message after, which is
	// its own default (bus.MemoryOptions.MaxAttempts 0): a run that still fails on it is
	// recorded as failed, because nothing will deliver the message again and the run
	// would show as running for ever.
	defaultFinalAttempt = 5
)

// Values of agents.runs.status.
const (
	statusQueued  = "queued"
	statusRunning = "running"
	statusDone    = "done"
	statusFailed  = "failed"
)

// protoJSON is how artifacts are stored in jsonb columns and events are carried on the
// outbox: field names as in the .proto, which is what outbox.Enqueue reads
// workspace_id by.
var protoJSON = protojson.MarshalOptions{UseProtoNames: true}

// Deps are what New and Jobs build the agents domain from.
type Deps struct {
	DB *db.DB
	// Authz decides who may ask a question over a channel.
	Authz *authz.Authz
	// Chat is where a run finds the message that started it and posts its reply.
	Chat chat.Service
	// Knowledge, Tasks and LLM are what the tools of a run act through.
	Knowledge knowledge.Service
	Tasks     tasks.Service
	LLM       llm.Client
	// Frames carries the progress frames of a run to the watchers; cache.Cache is the
	// production implementation.
	Frames outbox.FanoutPublisher
	// Runtime runs the agents. Nil means the ADK runtime over LLM.
	Runtime Runtime
	// RunTimeout bounds one attempt of a run; 0 means 120 s. A run that exceeds it is
	// failed at once, with reason "timeout", and not tried again.
	RunTimeout time.Duration
	// MaxTurns bounds the model calls of one attempt of the ADK runtime, the last of which
	// has to be the answer; 0 means 8. A run that exceeds it is failed at once, with
	// reason "too_many_steps".
	MaxTurns int
	// FinalAttempt is the delivery of agent.invoke that fails a run for good instead of
	// being tried again: it must equal the bus's MaxAttempts; 0 means 5, the bus's
	// default.
	FinalAttempt int
}

type service struct {
	db        *db.DB
	authz     *authz.Authz
	chat      chat.Service
	knowledge knowledge.Service
	tasks     tasks.Service
	llm       llm.Client
	frames    outbox.FanoutPublisher
	runtime   Runtime
	registry  map[string]Spec
	locks     *sessionLocks

	runTimeout   time.Duration
	finalAttempt int
}

// New returns the agents domain.
func New(d Deps) Service { return newService(d) }

// newService checks d and reads the registry. A missing dependency or a registry that
// does not load is a mistake of the program that wires the agents, so it panics, as
// NewTools does.
func newService(d Deps) *service {
	switch {
	case d.DB == nil:
		panic("agents: Deps.DB is required")
	case d.Authz == nil:
		panic("agents: Deps.Authz is required")
	case d.Chat == nil:
		panic("agents: Deps.Chat is required")
	case d.Knowledge == nil:
		panic("agents: Deps.Knowledge is required")
	case d.Tasks == nil:
		panic("agents: Deps.Tasks is required")
	case d.LLM == nil:
		panic("agents: Deps.LLM is required")
	case d.Frames == nil:
		panic("agents: Deps.Frames is required")
	}
	registry, err := LoadRegistry()
	if err != nil {
		panic(err)
	}
	rt := d.Runtime
	if rt == nil {
		rt = adkRuntime{llm: d.LLM, maxTurns: cmp.Or(d.MaxTurns, defaultMaxTurns)}
	}
	return &service{
		db: d.DB, authz: d.Authz, chat: d.Chat, knowledge: d.Knowledge, tasks: d.Tasks, llm: d.LLM,
		frames: d.Frames, runtime: rt, registry: registry, locks: newSessionLocks(),
		runTimeout: cmp.Or(d.RunTimeout, defaultRunTimeout), finalAttempt: cmp.Or(d.FinalAttempt, defaultFinalAttempt),
	}
}

var _ Service = (*service)(nil)

// StartRun implements Service. The run row and the agent.invoke event that carries it
// to the runtime are written in one transaction, so a run is never queued without its
// event nor announced without its row.
func (s *service) StartRun(ctx context.Context, a authz.Actor, req *agentsv1.StartRunRequest) (*agentsv1.Run, error) {
	if a.Kind != authz.ActorUser || a.WorkspaceID == uuid.Nil || a.UserID == uuid.Nil {
		return nil, fmt.Errorf("agents: only a signed-in person starts a run: %w", errs.ErrForbidden)
	}
	if _, ok := s.registry[req.GetAgent()]; !ok {
		return nil, fmt.Errorf("agents: there is no agent called %q: %w", req.GetAgent(), errs.ErrInvalid)
	}
	question := strings.TrimSpace(req.GetQuestion())
	if question == "" || utf8.RuneCountInString(question) > maxQuestionRunes || !utf8.ValidString(question) {
		return nil, fmt.Errorf("agents: a question is 1-%d characters of text: %w", maxQuestionRunes, errs.ErrInvalid)
	}
	var channel *uuid.UUID
	if req.ChannelId != nil {
		id, err := uuid.Parse(req.GetChannelId())
		if err != nil || id == uuid.Nil {
			return nil, fmt.Errorf("agents: channel id %q is not a UUID: %w", req.GetChannelId(), errs.ErrInvalid)
		}
		if err := s.authz.Can(ctx, a, authz.ActKnowledgeSearch, authz.Resource{ChannelID: id}); err != nil {
			return nil, err
		}
		channel = &id
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("agents: new run id: %w", err)
	}

	ev := &eventsv1.AgentInvoke{WorkspaceId: a.WorkspaceID.String(), RunId: id.String(), Agent: req.GetAgent(), OnBehalfOf: a.UserID.String()}
	if channel != nil {
		ev.ChannelId = channel.String()
	}
	payload, err := protoJSON.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("agents: encode %s event: %w", events.TopicAgentInvoke, err)
	}

	var row store.AgentsRun
	err = s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).InsertQueuedRun(ctx, store.InsertQueuedRunParams{
			ID: id, WorkspaceID: a.WorkspaceID, Agent: req.GetAgent(), ChannelID: channel, Question: question, OnBehalfOf: a.UserID,
		})
		if err != nil {
			return fmt.Errorf("insert run: %w", err)
		}
		return outbox.Enqueue(ctx, tx, outbox.Event{Topic: events.TopicAgentInvoke, Key: id.String(), Payload: payload})
	})
	if err != nil {
		return nil, fmt.Errorf("agents: start run: %w", err)
	}
	return runProto(row)
}

// GetRun implements Service. A run is known to the person who started it and to
// nobody else, however much of the workspace they share.
func (s *service) GetRun(ctx context.Context, a authz.Actor, runID uuid.UUID) (*agentsv1.Run, error) {
	if a.Kind != authz.ActorUser || a.WorkspaceID == uuid.Nil || a.UserID == uuid.Nil {
		return nil, fmt.Errorf("agents: only a signed-in person reads a run: %w", errs.ErrForbidden)
	}
	var row store.AgentsRun
	err := s.db.InWorkspace(ctx, a.WorkspaceID, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).GetRun(ctx, runID)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("agents: run %s: %w", runID, errs.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("agents: get run: %w", err)
	}
	if row.OnBehalfOf != a.UserID {
		return nil, fmt.Errorf("agents: run %s belongs to someone else: %w", runID, errs.ErrForbidden)
	}
	return runProto(row)
}

// runProto is a run as the API returns it.
func runProto(row store.AgentsRun) (*agentsv1.Run, error) {
	run := &agentsv1.Run{
		Id: row.ID.String(), Agent: row.Agent, Question: row.Question, Status: statusProto(row.Status),
	}
	if row.ChannelID != nil {
		run.ChannelId = proto.String(row.ChannelID.String())
	}
	if row.FailureReason != nil {
		run.FailureReason = *row.FailureReason
	}
	if row.Ref != nil {
		run.Ref = *row.Ref
	}
	if len(row.Result) > 0 {
		var answer chatv1.Artifact
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(row.Result, &answer); err != nil {
			return nil, fmt.Errorf("agents: decode the answer of run %s: %w", row.ID, err)
		}
		run.Answer = &answer
	}
	return run, nil
}

func statusProto(status string) agentsv1.RunStatus {
	switch status {
	case statusQueued:
		return agentsv1.RunStatus_RUN_STATUS_QUEUED
	case statusRunning:
		return agentsv1.RunStatus_RUN_STATUS_RUNNING
	case statusDone:
		return agentsv1.RunStatus_RUN_STATUS_DONE
	case statusFailed:
		return agentsv1.RunStatus_RUN_STATUS_FAILED
	}
	return agentsv1.RunStatus_RUN_STATUS_UNSPECIFIED
}
