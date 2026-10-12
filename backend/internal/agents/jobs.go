package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	"kiwi/internal/agents/internal/store"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/outbox"
	"kiwi/internal/platform/refs"
)

const (
	// consumerRun is the name of the job; it reads agent.invoke.
	consumerRun = "agent.run"
	// triggerPage and triggerPages bound the search for the message that started a
	// run: chat reads history by seq, not by id, and the message is among the latest.
	triggerPage  = 200
	triggerPages = 3
	// maxBodyRunes is the longest message chat accepts.
	maxBodyRunes = 8000
	// maxTitleRunes cuts the question that titles a slip.
	maxTitleRunes = 200
)

// Why a run failed, as agents.runs.failure_reason keeps it: a short code, as the failure
// reasons of files and meetings are, which a client words in the person's language. None
// repeats what a provider said: an error of the model path may name the URL it called.
const (
	reasonModelUnavailable = "model_unavailable" // the model or something it needs failed, on every attempt
	reasonTimeout          = "timeout"           // an attempt ran out of time (Deps.RunTimeout)
	reasonTooManySteps     = "too_many_steps"    // the model kept calling tools (Deps.MaxTurns)
	reasonTruncated        = "truncated"         // the answer ended at the token limit, twice
	reasonUnknownAgent     = "unknown_agent"     // the registry has no such agent
	reasonNoAccess         = "no_access"         // the person may not ask here any more, or the message is not theirs
	reasonTriggerTooOld    = "trigger_too_old"   // the message that called the agent is not among the latest
	reasonNoQuestion       = "no_question"       // the message has nothing to answer
	reasonInvalid          = "invalid"           // the event itself is wrong
)

var (
	// errEmptyAnswer is a run that ended with nothing to say and nothing to show.
	errEmptyAnswer = errors.New("the agent ended without an answer")
	// errMalformed is an event that is not an agent.invoke.
	errMalformed = errors.New("malformed agent.invoke")
	// errRunTimeout is an attempt that used up Deps.RunTimeout. The run is failed for it
	// at once: another attempt would take as long again, and would re-run the tools the
	// first one ran.
	errRunTimeout = errors.New("the run took too long")
	// errTriggerTooOld is a mention that is out of reach: chat is read back from the
	// newest message and the one that called the agent is further back than that.
	errTriggerTooOld = errors.New("the message that called the agent is no longer among the latest")
	// errRunEnded is the delivery that found its run ended by another one, when it came to
	// write its result: its transaction is rolled back, so it takes no REF and posts nothing.
	errRunEnded = errors.New("the run has already ended")
)

// Jobs returns the consumers of the agents domain for the composition root to
// register: agent.run, which runs the agent an agent.invoke event names for the person
// it names, and posts or stores what the agent answers.
func Jobs(d Deps) []bus.Job {
	s := newService(d)
	return []bus.Job{{Consumer: consumerRun, Topics: []string{events.TopicAgentInvoke}, Handler: s.handle}}
}

// invoke is an agent.invoke event.
type invoke struct {
	ws, run uuid.UUID
	agent   string
	user    uuid.UUID
	// channel and trigger are set for a mention run; channel alone for an Ask-panel
	// run that names a channel; neither for the rest.
	channel, trigger *uuid.UUID
}

func decodeInvoke(m bus.Message) (invoke, error) {
	var ev eventsv1.AgentInvoke
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(m.Data, &ev); err != nil {
		return invoke{}, fmt.Errorf("agents: decode %s: %v: %w", m.Topic, err, errs.ErrInvalid)
	}
	id := func(raw, what string) (uuid.UUID, error) {
		u, err := uuid.Parse(raw)
		if err != nil || u == uuid.Nil {
			return uuid.Nil, fmt.Errorf("agents: %s %q is not a UUID: %w: %w", what, raw, errMalformed, errs.ErrInvalid)
		}
		return u, nil
	}
	optional := func(raw, what string) (*uuid.UUID, error) {
		if raw == "" {
			return nil, nil
		}
		u, err := id(raw, what)
		return &u, err
	}
	var (
		inv invoke
		err error
	)
	inv.agent = ev.GetAgent()
	if inv.ws, err = id(ev.GetWorkspaceId(), "workspace id"); err != nil {
		return invoke{}, err
	}
	if attr := m.Attrs["workspace_id"]; attr != "" && attr != inv.ws.String() {
		return invoke{}, fmt.Errorf("agents: %s names workspace %s but was published for %s: %w", m.Topic, inv.ws, attr, errs.ErrInvalid)
	}
	if inv.run, err = id(ev.GetRunId(), "run id"); err != nil {
		return invoke{}, err
	}
	if inv.user, err = id(ev.GetOnBehalfOf(), "on_behalf_of"); err != nil {
		return invoke{}, err
	}
	if inv.channel, err = optional(ev.GetChannelId(), "channel id"); err != nil {
		return invoke{}, err
	}
	if inv.trigger, err = optional(ev.GetTriggerMessageId(), "trigger message id"); err != nil {
		return invoke{}, err
	}
	if inv.trigger != nil && inv.channel == nil {
		return invoke{}, fmt.Errorf("agents: %s has a trigger message and no channel: %w: %w", m.Topic, errMalformed, errs.ErrInvalid)
	}
	return inv, nil
}

// outcome is what a delivery settled, kept for the frame that follows its commit.
type outcome struct {
	progress *progress
	answer   *chatv1.Artifact
	failed   bool
	// applied is set by the Apply of the delivery that wrote the result; a delivery
	// that found the message processed already has nothing to announce.
	applied bool
}

// publish announces the result, once it is committed. It is best effort: the
// result is stored whether the frame reaches anyone or not.
func (o *outcome) publish(ctx context.Context) {
	switch {
	case !o.applied || o.progress == nil:
	case o.failed:
		o.progress.failed(ctx)
	default:
		o.progress.done(ctx, o.answer)
	}
}

// handle is the agent.run handler: the run, exactly once per message, and a last
// resort for the final delivery. A run that fails is delivered again until its last
// attempt, unless the failure is one no attempt gets past (see verdict); the last
// attempt records the failure so the run does not stay running for ever. A caller that
// has gone away, a shutdown, is not the run's fault and is delivered again.
func (s *service) handle(ctx context.Context, m bus.Message) error {
	oc := &outcome{}
	err := outbox.Once(s.db, consumerRun, func(ctx context.Context, m bus.Message) (outbox.Apply, error) {
		return s.prepare(ctx, m, oc)
	})(ctx, m)
	switch {
	case err == nil:
		oc.publish(ctx)
		return nil
	case errors.Is(err, errRunEnded):
		return nil // another delivery ended the run: its result stands, and there is nothing to retry
	case m.Attempt < s.finalAttempt || ctx.Err() != nil:
		return err
	}
	inv, derr := decodeInvoke(m)
	if derr != nil {
		return err // a message about no run: there is nothing to settle
	}
	reason, _ := verdict(err, m.Attempt)
	slog.WarnContext(ctx, "agents: giving up on a run at the final attempt", "run_id", inv.run, "attempt", m.Attempt, "reason", reason, "error", err)
	oc = &outcome{}
	if settle := outbox.OnceFor(ctx, s.db, consumerRun, m.ID, inv.ws, s.failing(inv, reason, oc)); settle != nil {
		return fmt.Errorf("agents: fail run %s at the final attempt: %w (the run failed with: %v)", inv.run, settle, err)
	}
	oc.publish(ctx)
	return nil
}

// verdict says why a run failed with err, and whether no further attempt can do better,
// so that the run is failed now: the person may not do this, what they asked about is
// gone, the run used up its time or its steps (another attempt would spend as much again
// and run the tools again), or an answer ended at the token limit a second time. Anything
// else is the model or its provider failing, which a later attempt may get past.
func verdict(err error, attempt int) (reason string, final bool) {
	switch {
	case errors.Is(err, errRunTimeout):
		return reasonTimeout, true
	case errors.Is(err, errTooManySteps):
		return reasonTooManySteps, true
	case errors.Is(err, errTriggerTooOld):
		return reasonTriggerTooOld, true
	case errors.Is(err, errs.ErrForbidden), errors.Is(err, errs.ErrNotFound):
		return reasonNoAccess, true
	case errors.Is(err, errs.ErrInvalid):
		return reasonInvalid, true
	case errors.Is(err, llm.ErrTruncated):
		return reasonTruncated, attempt > 1
	}
	return reasonModelUnavailable, false
}

// failing is the Apply that ends the run of inv as failed: it writes the row of a
// run that never got as far as having one, and leaves a run that ended as it ended (and
// announces nothing for it).
func (s *service) failing(inv invoke, reason string, oc *outcome) outbox.Apply {
	return func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.UpsertRunningRun(ctx, upsertParams(inv, "")); err != nil {
			return fmt.Errorf("agents: write run %s: %w", inv.run, err)
		}
		n, err := q.FailRun(ctx, store.FailRunParams{ID: inv.run, FailureReason: &reason})
		if err != nil {
			return fmt.Errorf("agents: fail run %s: %w", inv.run, err)
		}
		oc.progress = s.progressOf(inv.run, inv.trigger != nil, inv.channel, inv.user)
		oc.failed, oc.applied = true, n > 0
		return nil
	}
}

func upsertParams(inv invoke, question string) store.UpsertRunningRunParams {
	return store.UpsertRunningRunParams{
		ID: inv.run, WorkspaceID: inv.ws, Agent: inv.agent, ChannelID: inv.channel, TriggerMessageID: inv.trigger,
		Question: question, OnBehalfOf: inv.user,
	}
}

// progressOf is the progress of a run: on its channel when a message of the channel
// started it, on its user's topic otherwise.
func (s *service) progressOf(run uuid.UUID, mention bool, channel *uuid.UUID, user uuid.UUID) *progress {
	if !mention {
		channel = nil
	}
	return &progress{fan: s.frames, topic: progressTopic(channel, user), run: run}
}

// prepare is the agent.run handler's work for one message: the run itself, the slow
// part, outside the transaction that records the message. The Apply it returns writes
// the result and posts the reply.
func (s *service) prepare(ctx context.Context, m bus.Message, oc *outcome) (outbox.Apply, error) {
	inv, err := decodeInvoke(m)
	if err != nil {
		return nil, err
	}
	var row store.AgentsRun
	if err := s.db.InWorkspace(ctx, inv.ws, func(ctx context.Context, tx pgx.Tx) (err error) {
		row, err = store.New(tx).UpsertRunningRun(ctx, upsertParams(inv, ""))
		return err
	}); err != nil {
		return nil, fmt.Errorf("agents: start run %s: %w", inv.run, err)
	}
	if row.Status == statusDone || row.Status == statusFailed {
		return nil, nil // a run that ended is not run again, whatever message names it
	}
	// From here the row, not the event, says who the run is for and what it asks: a run
	// that exists is the one that was started, whatever a later event claims.
	inv.agent, inv.user, inv.channel, inv.trigger = row.Agent, row.OnBehalfOf, row.ChannelID, row.TriggerMessageID

	spec, ok := s.registry[row.Agent]
	if !ok {
		return s.failing(inv, reasonUnknownAgent, oc), nil
	}
	job, err := s.errand(ctx, inv, row)
	if err == nil && strings.TrimSpace(job.question) == "" {
		return s.failing(inv, reasonNoQuestion, oc), nil
	}
	if err != nil {
		if reason, stop := verdict(err, m.Attempt); stop {
			slog.WarnContext(ctx, "agents: run cannot start", "run_id", inv.run, "reason", reason, "error", err)
			return s.failing(inv, reason, oc), nil
		}
		return nil, err
	}

	oc.progress = s.progressOf(inv.run, job.trigger != nil, inv.channel, inv.user)
	oc.progress.running(ctx)
	final, col, err := s.execute(ctx, inv, spec, job, priorOf(row.ToolCalls), oc.progress)
	if err != nil {
		if reason, stop := verdict(err, m.Attempt); stop {
			slog.WarnContext(ctx, "agents: run stopped", "run_id", inv.run, "attempt", m.Attempt, "reason", reason, "error", err)
			return s.failing(inv, reason, oc), nil
		}
		return nil, err
	}

	slip := slipOf(col, job.question, inv.run, time.Now())
	text := strings.TrimSpace(strings.ToValidUTF8(final.Text, ""))
	if text == "" && slip != nil {
		text = slip.GetBodyMarkdown()
	}
	if text == "" {
		return nil, fmt.Errorf("agents: run %s: %w", inv.run, errEmptyAnswer)
	}
	return s.finish(inv, job, final, slip, text, oc), nil
}

// priorOf reads what the earlier attempts of a run did from its recorded tool calls: how
// many calls they made and which drafts. A call whose result was cut when it was
// recorded still counts as a draft; it just matches no call of a retry.
func priorOf(recorded json.RawMessage) Prior {
	var calls []ToolCall
	if len(recorded) == 0 || json.Unmarshal(recorded, &calls) != nil {
		return Prior{}
	}
	prior := Prior{Calls: len(calls)}
	seen := map[string]bool{}
	for _, c := range calls {
		if c.Name != toolCreateTaskDraft || c.Error != "" {
			continue
		}
		var d PriorDraft
		var res struct {
			Ref         string `json:"ref"`
			ChannelID   string `json:"channel_id"`
			Title       string `json:"title"`
			State       string `json:"state"`
			OwnerUserID string `json:"owner_user_id"`
			DueDate     string `json:"due_date"`
		}
		if json.Unmarshal([]byte(c.Result), &res) == nil {
			d = PriorDraft{ChannelID: res.ChannelID, Title: res.Title, Ref: res.Ref, State: res.State, OwnerUserID: res.OwnerUserID, DueDate: res.DueDate}
		}
		// A call that was handed a draft the run had made is recorded too, with the same
		// ref: it is one draft.
		if d.Ref != "" {
			if seen[d.Ref] {
				continue
			}
			seen[d.Ref] = true
		}
		prior.Drafts = append(prior.Drafts, d)
	}
	return prior
}

// errand is what a run answers.
type errand struct {
	// question is what is stored as the run's question; input is what the model reads.
	question, input string
	// trigger is the message of a mention run, nil for an Ask-panel run.
	trigger *Trigger
}

// errand finds what the run answers. For a mention run that is the message that started
// it, read as the person who wrote it: someone who is not in its channel any more, or
// whose message is not theirs, gets no answer. The event cannot make a run answer a
// message for someone else: the message has to be its author's.
func (s *service) errand(ctx context.Context, inv invoke, row store.AgentsRun) (errand, error) {
	if inv.trigger == nil {
		return errand{question: row.Question, input: askInput(row.Question, inv.channel)}, nil
	}
	person := authz.Actor{Kind: authz.ActorUser, WorkspaceID: inv.ws, UserID: inv.user}
	msg, err := s.findMessage(ctx, person, *inv.channel, *inv.trigger)
	if err != nil {
		return errand{}, err
	}
	if msg.GetAuthorKind() != chatv1.AuthorKind_AUTHOR_KIND_USER || msg.GetAuthorId() != inv.user.String() {
		return errand{}, fmt.Errorf("agents: message %s was not written by the person the run is for: %w", *inv.trigger, errs.ErrForbidden)
	}
	members, err := s.chat.ListMembers(ctx, person, *inv.channel)
	if err != nil {
		return errand{}, err
	}
	author := ""
	for _, mem := range members {
		if mem.GetUserId() == msg.GetAuthorId() {
			author = strings.TrimSpace(mem.GetName())
		}
	}
	body := clip(strings.TrimSpace(msg.GetBody()), maxQuestionRunes-1)
	if err := s.db.InWorkspace(ctx, inv.ws, func(ctx context.Context, tx pgx.Tx) error {
		return store.New(tx).SetRunQuestion(ctx, store.SetRunQuestionParams{ID: inv.run, Question: body})
	}); err != nil {
		return errand{}, fmt.Errorf("agents: record the question of run %s: %w", inv.run, err)
	}
	return errand{
		question: body,
		input:    mentionInput(*inv.channel, author, msg.GetMentions(), body),
		trigger: &Trigger{
			MessageID: *inv.trigger, ChannelID: *inv.channel, Seq: msg.GetSeq(), Author: author, At: msg.GetCreatedAt().AsTime(),
		},
	}, nil
}

// findMessage reads the message id of channel, which is among the latest: chat pages
// history by seq, so it walks back from the newest until it finds it, through
// triggerPages pages. A message further back, or that does not exist, is errTriggerTooOld.
func (s *service) findMessage(ctx context.Context, person authz.Actor, channel, id uuid.UUID) (*chatv1.Message, error) {
	var before int64
	for range triggerPages {
		page, err := s.chat.ListMessages(ctx, person, channel, before, triggerPage)
		if err != nil {
			return nil, err
		}
		for _, msg := range page {
			if msg.GetId() == id.String() {
				return msg, nil
			}
		}
		if len(page) < triggerPage {
			break
		}
		before = page[len(page)-1].GetSeq() // newest first: the last is the oldest
	}
	return nil, fmt.Errorf("agents: message %s of channel %s: %w", id, channel, errTriggerTooOld)
}

// execute runs the agent as the person the run is for, with the tools of this run, which
// start from what the run's earlier attempts used (prior). It holds the conversation while
// it runs, and its clock starts once it has it: one attempt may take s.runTimeout. A run of
// the Ask panel whose model left nothing to build a slip from (it answered from the
// conversation, or chatted) is searched for by the program, and what the model said is dropped.
func (s *service) execute(ctx context.Context, inv invoke, spec Spec, job errand, prior Prior, p *progress) (Final, *Collector, error) {
	actor := authz.Actor{Kind: authz.ActorAgent, WorkspaceID: inv.ws, OnBehalfOf: inv.user, Agent: spec.Name}
	col := &Collector{}
	tb := newToolbox(ToolDeps{
		Knowledge: s.knowledge, Tasks: s.tasks, Chat: s.chat, LLM: s.llm, Collector: col, Trigger: job.trigger, Channel: inv.channel,
		Prior: prior, Question: job.question,
	})
	sessionChannel := inv.channel
	if job.trigger == nil {
		sessionChannel = nil // the Ask panel has one conversation per person, whatever channel it names
	}
	sess := Session{
		ID: sessionID(spec.Name, inv.user, sessionChannel), UserID: inv.user.String(),
		Service: newSessionService(s.db, inv.ws), Tools: tb.tools(),
	}

	var final Final
	err := s.serialised(ctx, inv.ws, sess.ID, func(ctx context.Context) error {
		bus.KeepAlive(ctx)
		runCtx, cancel := context.WithTimeout(WithActor(ctx, actor), s.runTimeout)
		defer cancel()
		sess.Record = func(call ToolCall) error {
			if err := s.recordCall(runCtx, inv, call); err != nil {
				return err
			}
			p.running(runCtx) // what streamed before the call was not the answer
			return nil
		}
		var err error
		final, err = s.runtime.Run(runCtx, spec, job.input, sess, func(delta string) {
			bus.KeepAlive(runCtx)
			// An Ask is answered by the slip of a search, and a model that has not searched yet may be
			// answering from the conversation: its words are not previewed, as they are not kept.
			if job.trigger == nil && col.empty() {
				return
			}
			p.delta(runCtx, delta)
		})
		if err != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("agents: run %s: no answer within %s: %w", inv.run, s.runTimeout, errRunTimeout)
		}
		return err
	})
	if err != nil {
		return Final{}, nil, err
	}
	if job.trigger == nil && col.empty() {
		if err := s.search(ctx, inv, actor, tb); err != nil {
			return Final{}, nil, err
		}
		final.Text = ""
	}
	return final, col, nil
}

// search is the retrieval of a run of the Ask panel that ended without one: the program
// asks the knowledge domain itself, as the person's agent, with the person's own
// question, through the call search_knowledge makes. The call is recorded on the run,
// marked as forced, as the model's calls are. A person is answered from the retrieval of
// their own turn: the conversation only helps the model word a search. A retry of the run
// searches again by design: what an attempt carries over to the next is the budget its
// recorded calls spent (see priorOf), never the result.
func (s *service) search(ctx context.Context, inv invoke, actor authz.Actor, tb *toolbox) error {
	searchCtx, cancel := context.WithTimeout(WithActor(ctx, actor), s.runTimeout)
	defer cancel()
	bus.KeepAlive(searchCtx)
	call, made, err := tb.forceSearch(searchCtx)
	if made {
		if rerr := s.recordCall(ctx, inv, call); rerr != nil {
			return rerr
		}
	}
	if err != nil && errors.Is(searchCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("agents: run %s: no search within %s: %w", inv.run, s.runTimeout, errRunTimeout)
	}
	return err
}

// recordCall adds a tool call to the calls recorded on the run, as the call ends: they
// are the audit trail of the run, and what its later attempts count against the budget.
func (s *service) recordCall(ctx context.Context, inv invoke, call ToolCall) error {
	raw, err := json.Marshal([]ToolCall{call})
	if err != nil {
		return fmt.Errorf("agents: encode a tool call of run %s: %w", inv.run, err)
	}
	if err := s.db.InWorkspace(ctx, inv.ws, func(ctx context.Context, tx pgx.Tx) error {
		_, err := store.New(tx).AppendToolCall(ctx, store.AppendToolCallParams{ID: inv.run, Call: raw})
		return err
	}); err != nil {
		return fmt.Errorf("agents: record a tool call of run %s: %w", inv.run, err)
	}
	return nil
}

// finish returns the Apply that ends the run: it numbers the slip, posts the reply to
// the channel or keeps the slip for GetRun, and records the run's result, in the
// transaction that records the message. A run that another delivery ended meanwhile makes
// it fail with errRunEnded, which rolls back the REF it took and the reply it posted.
func (s *service) finish(inv invoke, job errand, final Final, slip *chatv1.Artifact, text string, oc *outcome) outbox.Apply {
	return func(ctx context.Context, tx pgx.Tx) error {
		var ref *string
		if slip != nil {
			n, err := refs.Next(ctx, tx, "ANS")
			if err != nil {
				return fmt.Errorf("agents: number the answer of run %s: %w", inv.run, err)
			}
			slip.Ref, ref = n, &n
		}
		if job.trigger != nil {
			actor := authz.Actor{Kind: authz.ActorAgent, WorkspaceID: inv.ws, OnBehalfOf: inv.user, Agent: inv.agent}
			if _, err := s.chat.PostMessageTx(ctx, tx, actor, &chatv1.SendMessageRequest{
				ChannelId: inv.channel.String(), ClientMsgId: replyID(inv.run), Body: clip(text, maxBodyRunes-1),
				AuthorId: proto.String(authz.AgentID(inv.agent).String()), Artifact: slip,
			}); err != nil {
				return fmt.Errorf("agents: post the reply of run %s: %w", inv.run, err)
			}
		}
		// An Ask-panel run has no channel to post to, and always has its slip (execute searches
		// when the model did not): the slip is its answer, and GetRun and the DONE frame give it.
		var result json.RawMessage
		if slip != nil {
			raw, err := protoJSON.Marshal(slip)
			if err != nil {
				return fmt.Errorf("agents: encode the answer of run %s: %w", inv.run, err)
			}
			result = raw
		}
		n, err := store.New(tx).CompleteRun(ctx, store.CompleteRunParams{
			ID: inv.run, Ref: ref, Result: result,
			InputTokens: tokens(final.Usage.InputTokens), OutputTokens: tokens(final.Usage.OutputTokens),
		})
		if err != nil {
			return fmt.Errorf("agents: complete run %s: %w", inv.run, err)
		}
		if n == 0 {
			return fmt.Errorf("agents: complete run %s: %w", inv.run, errRunEnded)
		}
		oc.answer, oc.applied = slip, true
		return nil
	}
}

// replyID is the client_msg_id of a run's reply. chat keys idempotency on
// (author_id, client_msg_id) globally, and every agent has the same author id in every
// workspace, so the id carries the run.
func replyID(run uuid.UUID) string { return "agent-run:" + run.String() }

func tokens(n int) int32 { return int32(min(max(n, 0), math.MaxInt32)) }

// slipOf builds the slip of what the run's tools returned, from the collector and
// never from what the model wrote, so a slip carries only citations that were checked.
// A found answer comes first, then a channel summary, then a search that found nothing
// (a completed search, whose slip says where it looked). A mention run that used none of
// those tools has no slip, and its reply is plain text; an Ask-panel run always has one
// (see execute). The Ref is given by the transaction that posts it.
func slipOf(col *Collector, question string, run uuid.UUID, now time.Time) *chatv1.Artifact {
	slip := func(kind chatv1.ArtifactKind) *chatv1.Artifact {
		return &chatv1.Artifact{
			Kind: kind, Title: clip(strings.TrimSpace(question), maxTitleRunes),
			GeneratedAt: timestamppb.New(now), RunId: proto.String(run.String()),
		}
	}
	ans, sum := col.Answer(), col.Summary()
	switch {
	case ans != nil && ans.GetFound():
		a := slip(chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER)
		a.BodyMarkdown, a.ScopeChannelIds = ans.GetMarkdown(), ans.GetScopeChannelIds()
		for _, c := range ans.GetCitations() {
			a.Citations = append(a.Citations, &chatv1.Citation{N: c.GetN(), ChunkId: c.GetChunkId(), Quote: c.GetQuote(), Source: c.GetSource()})
		}
		return a
	case sum != nil:
		a := slip(chatv1.ArtifactKind_ARTIFACT_KIND_CHANNEL_SUMMARY)
		a.BodyMarkdown, a.Sources, a.ScopeChannelIds = sum.Markdown, sum.Sources, []string{sum.ChannelID.String()}
		return a
	case ans != nil:
		a := slip(chatv1.ArtifactKind_ARTIFACT_KIND_NOT_FOUND)
		a.BodyMarkdown, a.ScopeChannelIds = ans.GetMarkdown(), ans.GetScopeChannelIds()
		return a
	}
	return nil
}

// mentionInput is what the model reads for a mention run: the message, with the ids
// its tools need and the model cannot know otherwise, the channel and the people the
// message names. It is data about the message, written by the program; the body that
// follows is the person's own.
func mentionInput(channel uuid.UUID, author string, mentions []*chatv1.Mention, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Context for your tools: this message was posted in the channel with id %s", channel)
	if author != "" {
		fmt.Fprintf(&b, " by %s", oneLine(author))
	}
	b.WriteString(".")
	var people []string
	for _, m := range mentions {
		if m.GetKind() == chatv1.MentionKind_MENTION_KIND_USER {
			people = append(people, fmt.Sprintf("%s = %s", oneLine(m.GetLabel()), m.GetId()))
		}
	}
	if len(people) > 0 {
		b.WriteString(" People it mentions, with their ids: " + strings.Join(people, "; ") + ".")
	}
	b.WriteString("]\n\n")
	b.WriteString(body)
	return b.String()
}

// askInput is what the model reads for an Ask-panel run: the question, with the reminder
// that every question is searched for, and the channel when the person asked about one.
// The reminder only makes a model that finds the answer in the conversation search anyway;
// the program does not rely on it (see execute).
func askInput(question string, channel *uuid.UUID) string {
	if channel == nil {
		return fmt.Sprintf("[Context for your tools: %s.]\n\n%s", askReminder, question)
	}
	return fmt.Sprintf("[Context for your tools: this question is about the channel with id %s; %s.]\n\n%s", channel, askReminder, question)
}

// askReminder is the sentence of askInput that sends a model to search first.
const askReminder = "this question comes from the Ask panel, so call search_knowledge for it first, even if the conversation above already answers it"

// oneLine makes a name safe to put in the bracketed context line.
func oneLine(s string) string {
	s = strings.NewReplacer("[", "(", "]", ")").Replace(s)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.Join(strings.Fields(s), " ")
}
