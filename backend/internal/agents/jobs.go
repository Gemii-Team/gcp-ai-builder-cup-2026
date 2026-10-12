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
	// finalAttempt is the delivery the bus dead-letters a message after: a run that
	// still fails on it is recorded as failed, because nothing will deliver the
	// message again and the run would show as running for ever.
	finalAttempt = 5
	// runTimeout bounds one attempt at a run: the model client has no timeout of its
	// own, and a model that runs away holds the run until the context ends.
	runTimeout = 3 * time.Minute
	// triggerPage and triggerPages bound the search for the message that started a
	// run: chat reads history by seq, not by id, and the message is among the latest.
	triggerPage  = 200
	triggerPages = 3
	// maxBodyRunes is the longest message chat accepts.
	maxBodyRunes = 8000
	// maxTitleRunes cuts the question that titles a slip.
	maxTitleRunes = 200
)

// Why a run failed, in the words a person reads. None repeats what the provider said:
// an error of the model path may name the URL it called.
const (
	reasonUnavailable  = "The agent could not answer right now. Try again."
	reasonTimeout      = "The agent took too long to answer. Try again."
	reasonTruncated    = "The agent's answer was cut off. Try a shorter question."
	reasonUnknownAgent = "This agent does not exist."
	reasonNoAccess     = "You can no longer ask this agent in this conversation."
	reasonNoQuestion   = "There was no question to answer."
)

var (
	// errEmptyAnswer is a run that ended with nothing to say and nothing to show.
	errEmptyAnswer = errors.New("the agent ended without an answer")
	// errMalformed is an event that is not an agent.invoke.
	errMalformed = errors.New("malformed agent.invoke")
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
// attempt; the last one records the failure so the run does not stay running for
// ever. A caller that has gone away, a shutdown, is not the run's fault and is
// delivered again.
func (s *service) handle(ctx context.Context, m bus.Message) error {
	oc := &outcome{}
	err := outbox.Once(s.db, consumerRun, func(ctx context.Context, m bus.Message) (outbox.Apply, error) {
		return s.prepare(ctx, m, oc)
	})(ctx, m)
	if err == nil {
		oc.publish(ctx)
		return nil
	}
	if m.Attempt < finalAttempt || ctx.Err() != nil {
		return err
	}
	inv, derr := decodeInvoke(m)
	if derr != nil {
		return err // a message about no run: there is nothing to settle
	}
	slog.WarnContext(ctx, "agents: giving up on a run at the final attempt", "run_id", inv.run, "attempt", m.Attempt, "error", err)
	oc = &outcome{}
	if settle := outbox.OnceFor(ctx, s.db, consumerRun, m.ID, inv.ws, s.failing(inv, failureReason(err), oc)); settle != nil {
		return fmt.Errorf("agents: fail run %s at the final attempt: %w (the run failed with: %v)", inv.run, settle, err)
	}
	oc.publish(ctx)
	return nil
}

// failureReason is what to tell the person about err.
func failureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return reasonTimeout
	case errors.Is(err, llm.ErrTruncated):
		return reasonTruncated
	case errors.Is(err, errs.ErrForbidden), errors.Is(err, errs.ErrNotFound):
		return reasonNoAccess
	}
	return reasonUnavailable
}

// permanent reports an error that no delivery will get past: the person may not do
// this, or the thing they asked about is gone.
func permanent(err error) bool {
	return errors.Is(err, errs.ErrForbidden) || errors.Is(err, errs.ErrNotFound) || errors.Is(err, errs.ErrInvalid)
}

// failing is the Apply that ends the run of inv as failed: it writes the row of a
// run that never got as far as having one, and leaves a run that ended as it ended.
func (s *service) failing(inv invoke, reason string, oc *outcome) outbox.Apply {
	return func(ctx context.Context, tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := q.UpsertRunningRun(ctx, upsertParams(inv, "")); err != nil {
			return fmt.Errorf("agents: write run %s: %w", inv.run, err)
		}
		if _, err := q.FailRun(ctx, store.FailRunParams{ID: inv.run, FailureReason: &reason}); err != nil {
			return fmt.Errorf("agents: fail run %s: %w", inv.run, err)
		}
		oc.progress = s.progressOf(inv.run, inv.trigger != nil, inv.channel, inv.user)
		oc.failed, oc.applied = true, true
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
	// From here the row, not the event, says who the run is for and what it asks.
	inv.agent, inv.user, inv.channel, inv.trigger = row.Agent, row.OnBehalfOf, row.ChannelID, row.TriggerMessageID

	spec, ok := s.registry[row.Agent]
	if !ok {
		return s.failing(inv, reasonUnknownAgent, oc), nil
	}
	job, err := s.errand(ctx, inv, row)
	switch {
	case permanent(err):
		slog.WarnContext(ctx, "agents: run cannot start", "run_id", inv.run, "error", err)
		return s.failing(inv, failureReason(err), oc), nil
	case err != nil:
		return nil, err
	case strings.TrimSpace(job.question) == "":
		return s.failing(inv, reasonNoQuestion, oc), nil
	}

	oc.progress = s.progressOf(inv.run, job.trigger != nil, inv.channel, inv.user)
	oc.progress.started(ctx)
	final, col, err := s.execute(ctx, inv, spec, job, oc.progress)
	if err != nil {
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

// errand is what a run answers.
type errand struct {
	// question is what is stored as the run's question; input is what the model reads.
	question, input string
	// trigger is the message of a mention run, nil for an Ask-panel run.
	trigger *Trigger
}

// errand finds what the run answers. For a mention run that is the message that started
// it, read as the person who wrote it: someone who is not in its channel any more, or
// whose message is not theirs, gets no answer.
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
	body := strings.TrimSpace(msg.GetBody())
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
// history by seq, so it walks back from the newest until it finds it.
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
	return nil, fmt.Errorf("agents: message %s is not among the latest of channel %s: %w", id, channel, errs.ErrNotFound)
}

// execute runs the agent as the person the run is for, with the tools of this run.
func (s *service) execute(ctx context.Context, inv invoke, spec Spec, t errand, p *progress) (Final, *Collector, error) {
	actor := authz.Actor{Kind: authz.ActorAgent, WorkspaceID: inv.ws, OnBehalfOf: inv.user, Agent: spec.Name}
	col := &Collector{}
	tools := NewTools(ToolDeps{Knowledge: s.knowledge, Tasks: s.tasks, Chat: s.chat, LLM: s.llm, Collector: col, Trigger: t.trigger})
	sessionChannel := inv.channel
	if t.trigger == nil {
		sessionChannel = nil // the Ask panel has one conversation per person, whatever channel it names
	}
	sess := Session{
		ID: sessionID(spec.Name, inv.user, sessionChannel), UserID: inv.user.String(),
		Service: newSessionService(s.db, inv.ws), Tools: tools,
	}

	ctx, cancel := context.WithTimeout(WithActor(ctx, actor), runTimeout)
	defer cancel()
	bus.KeepAlive(ctx)
	final, err := s.runtime.Run(ctx, spec, t.input, sess, func(delta string) {
		bus.KeepAlive(ctx)
		p.delta(ctx, delta)
	})
	if err != nil {
		return Final{}, nil, err
	}
	return final, col, nil
}

// finish returns the Apply that ends the run: it numbers the slip, posts the reply to
// the channel or keeps the answer for GetRun, and records the run's result, in the
// transaction that records the message.
func (s *service) finish(inv invoke, t errand, final Final, slip *chatv1.Artifact, text string, oc *outcome) outbox.Apply {
	return func(ctx context.Context, tx pgx.Tx) error {
		var ref *string
		if slip != nil {
			n, err := refs.Next(ctx, tx, "ANS")
			if err != nil {
				return fmt.Errorf("agents: number the answer of run %s: %w", inv.run, err)
			}
			slip.Ref, ref = n, &n
		}
		var result json.RawMessage
		answer := slip
		if t.trigger != nil {
			actor := authz.Actor{Kind: authz.ActorAgent, WorkspaceID: inv.ws, OnBehalfOf: inv.user, Agent: inv.agent}
			if _, err := s.chat.PostMessageTx(ctx, tx, actor, &chatv1.SendMessageRequest{
				ChannelId: inv.channel.String(), ClientMsgId: replyID(inv.run), Body: clip(text, maxBodyRunes-1),
				AuthorId: proto.String(authz.AgentID(inv.agent).String()), Artifact: slip,
			}); err != nil {
				return fmt.Errorf("agents: post the reply of run %s: %w", inv.run, err)
			}
		} else if answer == nil {
			// An Ask-panel run has no channel to post to: its text is its answer.
			answer = &chatv1.Artifact{BodyMarkdown: text, GeneratedAt: timestamppb.Now(), RunId: proto.String(inv.run.String())}
		}
		if answer != nil {
			raw, err := protoJSON.Marshal(answer)
			if err != nil {
				return fmt.Errorf("agents: encode the answer of run %s: %w", inv.run, err)
			}
			result = raw
		}
		calls := final.ToolCalls
		if calls == nil {
			calls = []ToolCall{}
		}
		toolCalls, err := json.Marshal(calls)
		if err != nil {
			return fmt.Errorf("agents: encode the tool calls of run %s: %w", inv.run, err)
		}
		if err := store.New(tx).CompleteRun(ctx, store.CompleteRunParams{
			ID: inv.run, Ref: ref, Result: result, ToolCalls: toolCalls,
			InputTokens: tokens(final.Usage.InputTokens), OutputTokens: tokens(final.Usage.OutputTokens),
		}); err != nil {
			return fmt.Errorf("agents: complete run %s: %w", inv.run, err)
		}
		oc.answer, oc.applied = answer, true
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
// (a completed search, whose slip says where it looked). A run that used none of those
// tools has no slip; the Ref is given by the transaction that posts it.
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

// askInput is what the model reads for an Ask-panel run: the question, and the
// channel when the person asked about one.
func askInput(question string, channel *uuid.UUID) string {
	if channel == nil {
		return question
	}
	return fmt.Sprintf("[Context for your tools: this question is about the channel with id %s.]\n\n%s", channel, question)
}

// oneLine makes a name safe to put in the bracketed context line.
func oneLine(s string) string {
	s = strings.NewReplacer("[", "(", "]", ")").Replace(s)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.Join(strings.Fields(s), " ")
}
