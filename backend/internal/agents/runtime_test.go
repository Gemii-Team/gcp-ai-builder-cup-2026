package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentsv1 "kiwi/gen/kiwi/agents/v1"
	"kiwi/gen/kiwi/agents/v1/agentsv1connect"
	chatv1 "kiwi/gen/kiwi/chat/v1"
	eventsv1 "kiwi/gen/kiwi/events/v1"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	"kiwi/internal/chat"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/cache"
	"kiwi/internal/platform/config"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/llm"
	"kiwi/internal/platform/llm/openai"
	"kiwi/internal/testkit/containers"
)

// ---- Valkey -----------------------------------------------------------------

// One plain Valkey serves the package; every test follows topics made from fresh UUIDs.
var (
	valkeyOnce sync.Once
	valkeyURL  string
	valkeyErr  error
	valkeyCtr  testcontainers.Container
)

func TestMain(m *testing.M) {
	code := m.Run()
	if valkeyCtr != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = valkeyCtr.Terminate(ctx)
		cancel()
	}
	os.Exit(code)
}

func bootValkey() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctr, err := containers.Start(ctx, "valkey/valkey:8",
		testcontainers.WithLogger(log.New(io.Discard, "", 0)),
		testcontainers.WithExposedPorts("6379/tcp"),
		testcontainers.WithCmd("valkey-server", "--save", "", "--appendonly", "no"),
		testcontainers.WithWaitStrategy(wait.ForLog("Ready to accept connections")),
	)
	if err != nil {
		return "", fmt.Errorf("start valkey (is the Docker daemon reachable?): %w", err)
	}
	valkeyCtr = ctr
	host, err := ctr.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := ctr.MappedPort(ctx, "6379/tcp")
	if err != nil {
		return "", err
	}
	return "redis://" + host + ":" + port.Port(), nil
}

func openCache(t *testing.T) *cache.Cache {
	t.Helper()
	valkeyOnce.Do(func() { valkeyURL, valkeyErr = bootValkey() })
	if valkeyErr != nil {
		t.Fatalf("valkey: %v", valkeyErr)
	}
	c, err := cache.Open(t.Context(), valkeyURL, cache.Options{})
	if err != nil {
		t.Fatalf("cache.Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---- logs and workspaces -----------------------------------------------------

// syncBuf is a log sink the runtime may write to from any goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLogs sends the default logger to the returned buffer for the rest of the
// test, so a warning a test provokes is asserted and never printed.
func captureLogs(t *testing.T) *syncBuf {
	t.Helper()
	prev := slog.Default()
	buf := &syncBuf{}
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// newWorkspace makes a second workspace, as the owner bound to it.
func (f *fixture) newWorkspace() uuid.UUID {
	f.t.Helper()
	ctx := f.t.Context()
	id := uuid.New()
	conn, err := pgx.Connect(ctx, f.env.OwnerURL)
	if err != nil {
		f.t.Fatalf("connect as owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('kiwi.workspace_id', $1, true)`, id.String()); err != nil {
		f.t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity.workspaces (id, name, google_domain) VALUES ($1, 'other', 'other.example')`, id); err != nil {
		f.t.Fatalf("insert workspace: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// ---- the scripted model ------------------------------------------------------

// step is one answer of the scripted model: a call of a tool, text, or a failure.
type step struct {
	call *genai.FunctionCall
	// text is streamed in pieces of chunk runes when the caller streams.
	text string
	err  error
	// hold, when set, blocks a streaming answer after its first piece until it is closed.
	hold <-chan struct{}
	// block makes the call wait until its context ends, as a model that never answers.
	block bool
	// truncated ends the answer at the token limit.
	truncated bool
	// forever keeps the step at the head of the script: the model plays it on every call.
	forever bool
}

// say is a step that answers with text.
func say(text string) step { return step{text: text} }

// callIDs makes the id of every call of a tool unique, as a provider's are.
var callIDs atomic.Int64

// callTool is a step that calls a tool.
func callTool(name string, args map[string]any) step {
	return step{call: &genai.FunctionCall{Name: name, Args: args}}
}

// scriptedModel is an ADK model that plays its steps in order, one per model call,
// and keeps every request it was asked.
type scriptedModel struct {
	mu    sync.Mutex
	steps []step
	reqs  []*model.LLMRequest
	tiers []llm.Tier
}

func (m *scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) script(steps ...step) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, steps...)
}

// reset drops the steps that are left, those that repeat forever included.
func (m *scriptedModel) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = nil
}

func (m *scriptedModel) requests() []*model.LLMRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.reqs)
}

func (m *scriptedModel) tierList() []llm.Tier {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.tiers)
}

func (m *scriptedModel) usage() *genai.GenerateContentResponseUsageMetadata {
	return &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 120, CandidatesTokenCount: 30, TotalTokenCount: 150}
}

func (m *scriptedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	var s step
	ok := len(m.steps) > 0
	if ok {
		s = m.steps[0]
		if !s.forever {
			m.steps = m.steps[1:]
		}
	}
	m.mu.Unlock()

	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case !ok:
			yield(nil, errors.New("scripted model: no step left for this call"))
		case s.err != nil:
			yield(nil, s.err)
		case s.block:
			<-ctx.Done()
			yield(nil, ctx.Err())
		case s.call != nil:
			// A call of its own on every play: ADK edits the ids of the calls it is given.
			call := &genai.FunctionCall{ID: fmt.Sprintf("call-%d", callIDs.Add(1)), Name: s.call.Name, Args: maps.Clone(s.call.Args)}
			yield(&model.LLMResponse{
				Content:       &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: call}}},
				UsageMetadata: m.usage(), TurnComplete: true,
			}, nil)
		default:
			if stream {
				pieces := chunks(s.text, 8)
				for i, p := range pieces {
					if !yield(&model.LLMResponse{Content: genai.NewContentFromText(p, genai.RoleModel), Partial: true}, nil) {
						return
					}
					if i == 0 && s.hold != nil {
						select {
						case <-s.hold:
						case <-ctx.Done():
							return
						}
					}
				}
			}
			final := &model.LLMResponse{
				Content: genai.NewContentFromText(s.text, genai.RoleModel), UsageMetadata: m.usage(), TurnComplete: true,
			}
			if s.truncated {
				final.FinishReason = genai.FinishReasonMaxTokens
			}
			yield(final, nil)
		}
	}
}

// chunks cuts s into pieces of n runes.
func chunks(s string, n int) []string {
	var out []string
	r := []rune(s)
	for len(r) > 0 {
		k := min(n, len(r))
		out = append(out, string(r[:k]))
		r = r[k:]
	}
	return out
}

// ---- the agent fixture -------------------------------------------------------

// agentFixture is the fixture of the tools with the runtime over it: a real Valkey
// for progress, the scripted model behind the fake llm's ADK path, and a chat service
// that keeps @assistant mentions, as the one of the api does.
type agentFixture struct {
	*fixture
	cache    *cache.Cache
	model    *scriptedModel
	mentions chat.Service
	deps     Deps
}

func newAgentFixture(t *testing.T) *agentFixture {
	t.Helper()
	f := newFixture(t)
	m := &scriptedModel{}
	f.llm.HandleADKModel(func(tier llm.Tier) (model.LLM, error) {
		m.mu.Lock()
		m.tiers = append(m.tiers, tier)
		m.mu.Unlock()
		return m, nil
	})
	az := authz.New(sqlMembership{db: f.db})
	c := openCache(t)
	return &agentFixture{
		fixture: f, cache: c, model: m,
		mentions: chat.New(chat.Deps{DB: f.db, Authz: az, KnownAgent: func(name string) bool { return name == "assistant" }}),
		deps: Deps{
			DB: f.db, Authz: az, Chat: f.chat, Knowledge: f.kb, Tasks: f.tasks, LLM: f.llm, Frames: c,
		},
	}
}

// job is the agent.run handler of a fresh process.
func (a *agentFixture) job() bus.Handler {
	jobs := Jobs(a.deps)
	if len(jobs) != 1 {
		a.t.Fatalf("Jobs returned %d consumers, want 1", len(jobs))
	}
	return jobs[0].Handler
}

// mention posts body in channel as author with an @assistant mention, as the
// composer does.
func (a *agentFixture) mention(channel, author uuid.UUID, body string) *chatv1.Message {
	a.t.Helper()
	msg, err := a.mentions.PostMessage(a.t.Context(), a.person(author), &chatv1.SendMessageRequest{
		ChannelId: channel.String(), ClientMsgId: uuid.NewString(), Body: body,
		Mentions: []*chatv1.Mention{{Kind: chatv1.MentionKind_MENTION_KIND_AGENT, Id: authz.AgentID("assistant").String(), Label: "assistant"}},
	})
	if err != nil {
		a.t.Fatalf("PostMessage: %v", err)
	}
	return msg
}

var wireJSON = protojson.MarshalOptions{UseProtoNames: true}

// invokeOf is the agent.invoke event the router makes of a mention message.
func (a *agentFixture) invokeOf(msg *chatv1.Message, run uuid.UUID) *eventsv1.AgentInvoke {
	return &eventsv1.AgentInvoke{
		WorkspaceId: a.ws.String(), RunId: run.String(), Agent: "assistant",
		ChannelId: msg.GetChannelId(), TriggerMessageId: msg.GetId(), OnBehalfOf: msg.GetAuthorId(),
	}
}

// delivery is the bus message of an agent.invoke event; id is what exactly-once is
// keyed on and attempt the delivery count.
func (a *agentFixture) delivery(ev *eventsv1.AgentInvoke, id string, attempt int) bus.Message {
	a.t.Helper()
	data, err := wireJSON.Marshal(ev)
	if err != nil {
		a.t.Fatal(err)
	}
	return bus.Message{
		ID: id, Topic: events.TopicAgentInvoke, Key: ev.GetRunId(), Data: data, Attempt: attempt,
		Attrs: map[string]string{"workspace_id": ev.GetWorkspaceId(), "topic": events.TopicAgentInvoke},
	}
}

// replies are the messages agents posted in channel, newest first.
func (a *agentFixture) replies(channel, as uuid.UUID) []*chatv1.Message {
	a.t.Helper()
	msgs, err := a.chat.ListMessages(a.t.Context(), a.person(as), channel, 0, 50)
	if err != nil {
		a.t.Fatalf("ListMessages: %v", err)
	}
	var out []*chatv1.Message
	for _, m := range msgs {
		if m.GetAuthorKind() == chatv1.AuthorKind_AUTHOR_KIND_AGENT {
			out = append(out, m)
		}
	}
	return out
}

// runRow is agents.runs as the tests read it.
type runRow struct {
	Status, Question, Agent   string
	Ref, FailureReason        *string
	Result, ToolCalls         []byte
	InputTokens, OutputTokens int
	OnBehalfOf                uuid.UUID
	ChannelID, TriggerID      *uuid.UUID
}

func (a *agentFixture) run(id uuid.UUID) runRow {
	a.t.Helper()
	var r runRow
	a.scan(&r.Status, `SELECT status FROM agents.runs WHERE id = $1`, id)
	a.scan(&r.Question, `SELECT question FROM agents.runs WHERE id = $1`, id)
	a.scan(&r.Agent, `SELECT agent FROM agents.runs WHERE id = $1`, id)
	a.scan(&r.OnBehalfOf, `SELECT on_behalf_of FROM agents.runs WHERE id = $1`, id)
	a.scan(&r.InputTokens, `SELECT input_tokens FROM agents.runs WHERE id = $1`, id)
	a.scan(&r.OutputTokens, `SELECT output_tokens FROM agents.runs WHERE id = $1`, id)
	var ref, reason *string
	a.scan(&ref, `SELECT ref FROM agents.runs WHERE id = $1`, id)
	a.scan(&reason, `SELECT failure_reason FROM agents.runs WHERE id = $1`, id)
	r.Ref, r.FailureReason = ref, reason
	var result, calls string
	a.scan(&result, `SELECT COALESCE(result::text, '') FROM agents.runs WHERE id = $1`, id)
	a.scan(&calls, `SELECT tool_calls::text FROM agents.runs WHERE id = $1`, id)
	r.Result, r.ToolCalls = []byte(result), []byte(calls)
	a.scan(&r.ChannelID, `SELECT channel_id FROM agents.runs WHERE id = $1`, id)
	a.scan(&r.TriggerID, `SELECT trigger_message_id FROM agents.runs WHERE id = $1`, id)
	return r
}

func (a *agentFixture) outboxRows() int {
	return a.count(`SELECT count(*) FROM platform.outbox`)
}

// ---- progress frames over Valkey ---------------------------------------------

// frameWatch collects the AgentRunProgress frames of one Valkey topic.
type frameWatch struct {
	t      *testing.T
	mu     sync.Mutex
	frames []*realtimev1.AgentRunProgress
	live   bool
}

// watch follows topic and returns once the subscription is live: it publishes probes
// until the first one arrives, so no frame of the test can be missed.
func (a *agentFixture) watch(topic string) *frameWatch {
	a.t.Helper()
	sub, err := a.cache.SSubscribe(a.t.Context())
	if err != nil {
		a.t.Fatalf("SSubscribe: %v", err)
	}
	a.t.Cleanup(sub.Close)
	w := &frameWatch{t: a.t}
	go func() {
		for f := range sub.C() {
			var ev realtimev1.ServerEvent
			if err := proto.Unmarshal(f.Data, &ev); err != nil {
				continue
			}
			w.mu.Lock()
			w.live = true
			if p := ev.GetAgentRunProgress(); p != nil {
				w.frames = append(w.frames, p)
			}
			w.mu.Unlock()
		}
	}()
	sub.Add(topic)
	probe, err := proto.Marshal(&realtimev1.ServerEvent{Event: &realtimev1.ServerEvent_Presence{Presence: &realtimev1.Presence{UserId: "probe"}}})
	if err != nil {
		a.t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.mu.Lock()
		live := w.live
		w.mu.Unlock()
		if live {
			return w
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("the subscription to %s never went live", topic)
		}
		if err := a.cache.SPublish(a.t.Context(), topic, probe); err != nil {
			a.t.Fatalf("SPublish probe: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitFor returns the frames seen once cond holds of them, or fails after 10 s.
func (w *frameWatch) waitFor(what string, cond func([]*realtimev1.AgentRunProgress) bool) []*realtimev1.AgentRunProgress {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w.mu.Lock()
		got := slices.Clone(w.frames)
		w.mu.Unlock()
		if cond(got) {
			return got
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("waiting for %s: %d frames seen: %v", what, len(got), got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (w *frameWatch) snapshot() []*realtimev1.AgentRunProgress {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.frames)
}

func hasState(state realtimev1.AgentRunState) func([]*realtimev1.AgentRunProgress) bool {
	return func(frames []*realtimev1.AgentRunProgress) bool {
		return slices.ContainsFunc(frames, func(p *realtimev1.AgentRunProgress) bool { return p.GetState() == state })
	}
}

// ---- mention runs ------------------------------------------------------------

const warehouseFact = "The Bang Na warehouse holds 4,000 pallets."

// seedWarehouse makes the knowledge index able to answer a pallets question in the
// public channel, with a fake model that cites every chunk it is shown.
func (a *agentFixture) seedWarehouse() {
	a.llm.HandleGenerate(citeEveryChunk)
	a.indexedMeeting(a.public, a.bob, "Warehouse review", warehouseFact)
}

func TestInvoke_MentionRun_PostsAgentReplyWithValidatedAnswerArtifact(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": "How many pallets does the warehouse hold?"}),
		say("The warehouse holds 4,000 pallets [1]."),
	)
	trigger := a.mention(a.public, a.alice, "@assistant how many pallets does the warehouse hold?")
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}

	replies := a.replies(a.public, a.alice)
	if len(replies) != 1 {
		t.Fatalf("agent replies = %d, want 1", len(replies))
	}
	reply := replies[0]
	if reply.GetAuthorId() != authz.AgentID("assistant").String() {
		t.Errorf("author_id = %s, want authz.AgentID(assistant) %s", reply.GetAuthorId(), authz.AgentID("assistant"))
	}
	if reply.GetBody() != "The warehouse holds 4,000 pallets [1]." {
		t.Errorf("body = %q, want the model's text", reply.GetBody())
	}
	if want := "agent-run:" + runID.String(); reply.GetClientMsgId() != want {
		t.Errorf("client_msg_id = %q, want %q: unique to the run, since the key is global", reply.GetClientMsgId(), want)
	}
	art := reply.GetArtifact()
	if art.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || art.GetRef() != "ANS-1001" {
		t.Fatalf("artifact = %v, want an ANSWER with ANS-1001", art)
	}
	if art.GetRunId() != runID.String() {
		t.Errorf("artifact run_id = %q, want the run %s", art.GetRunId(), runID)
	}
	if len(art.GetCitations()) == 0 {
		t.Fatalf("artifact has no citations: %v", art)
	}
	for _, c := range art.GetCitations() {
		if c.GetSource().GetChannelId() != a.public.String() || c.GetSource().GetLabel() != "Warehouse review" ||
			c.GetSource().GetKind() != chatv1.SourceKind_SOURCE_KIND_MEETING {
			t.Errorf("citation %d = %v, want the warehouse meeting of the public channel, as the collector held it", c.GetN(), c)
		}
	}
	if !slices.Equal(art.GetScopeChannelIds(), idStrings(a.public)) {
		t.Errorf("scope_channel_ids = %v, want only the channels alice belongs to: %v", art.GetScopeChannelIds(), a.public)
	}
	if !strings.Contains(art.GetBodyMarkdown(), "[1]") {
		t.Errorf("artifact body = %q, want the validated answer of the search", art.GetBodyMarkdown())
	}

	run := a.run(runID)
	if run.Status != "done" || run.Ref == nil || *run.Ref != "ANS-1001" || run.FailureReason != nil {
		t.Errorf("run = %+v, want done with ANS-1001", run)
	}
	if run.OnBehalfOf != a.alice || run.Agent != "assistant" || run.Question != trigger.GetBody() {
		t.Errorf("run on_behalf_of/agent/question = %v/%s/%q, want alice/assistant/the mention", run.OnBehalfOf, run.Agent, run.Question)
	}
	if run.ChannelID == nil || *run.ChannelID != a.public || run.TriggerID == nil || trigger.GetId() != run.TriggerID.String() {
		t.Errorf("run channel/trigger = %v/%v, want the channel and the mention", run.ChannelID, run.TriggerID)
	}
	if run.InputTokens <= 0 || run.OutputTokens <= 0 {
		t.Errorf("tokens = %d in / %d out, want counts from both model calls", run.InputTokens, run.OutputTokens)
	}
	var calls []struct {
		Name   string         `json:"name"`
		Args   map[string]any `json:"args"`
		Result string         `json:"result"`
		Error  string         `json:"error"`
	}
	if err := json.Unmarshal(run.ToolCalls, &calls); err != nil || len(calls) != 1 || calls[0].Name != "search_knowledge" ||
		calls[0].Args["question"] == nil || !strings.Contains(calls[0].Result, `"found":true`) || !strings.Contains(calls[0].Result, "Warehouse review") || calls[0].Error != "" {
		t.Errorf("tool_calls = %s (%v), want the one search_knowledge call with its arguments and result", run.ToolCalls, err)
	}
	var stored chatv1.Artifact
	if err := protojson.Unmarshal(run.Result, &stored); err != nil || stored.GetRef() != "ANS-1001" {
		t.Errorf("run result = %s (%v), want the artifact that was posted", run.Result, err)
	}

	// How the model was run: the tier of the registry entry, bounded output, the
	// registry's instruction, and the context of the mention.
	if tiers := a.model.tierList(); len(tiers) != 1 || tiers[0] != llm.TierFlash {
		t.Errorf("model tiers asked = %v, want flash once", tiers)
	}
	reqs := a.model.requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2 (the tool call, then the answer)", len(reqs))
	}
	first := reqs[0]
	if first.Config == nil || first.Config.MaxOutputTokens < 1100 {
		t.Errorf("model config = %+v, want MaxOutputTokens of at least 1100: a thinking model spends less than that on reasoning", first.Config)
	}
	if first.Config.SystemInstruction == nil || !strings.Contains(first.Config.SystemInstruction.Parts[0].Text, "You are @assistant") {
		t.Errorf("system instruction = %v, want the registry's", first.Config.SystemInstruction)
	}
	var seen strings.Builder
	for _, c := range first.Contents {
		for _, p := range c.Parts {
			seen.WriteString(p.Text)
		}
	}
	for _, want := range []string{a.public.String(), "how many pallets does the warehouse hold?", "Alice"} {
		if !strings.Contains(seen.String(), want) {
			t.Errorf("the model was shown %q, want it to name %q", seen.String(), want)
		}
	}
}

func TestInvoke_RedeliveryPostsOneReply(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": "pallets?"}),
		say("4,000 pallets [1]."),
	)
	trigger := a.mention(a.public, a.alice, "@assistant pallets?")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)
	job := a.job()

	if err := job(t.Context(), a.delivery(ev, "evt-1", 1)); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	// The bus delivers the same message again, to a process that restarted meanwhile.
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 2)); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	// The relay publishes the same run under another message id.
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1-again", 1)); err != nil {
		t.Fatalf("second message of the run: %v", err)
	}

	if n := len(a.replies(a.public, a.alice)); n != 1 {
		t.Errorf("agent replies = %d, want 1", n)
	}
	if n := len(a.model.requests()); n != 2 {
		t.Errorf("model calls = %d, want 2: a run that is done is not run again", n)
	}
	if n := a.count(`SELECT count(*) FROM platform.ref_counters WHERE kind = 'ANS' AND last = 1001`); n != 1 {
		t.Errorf("ANS counter rows at 1001 = %d, want 1: one REF for the one answer", n)
	}
}

func TestInvoke_ProgressGoesToValkeyNotOutbox(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	release := make(chan struct{})
	text := "The warehouse holds 4,000 pallets [1]."
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": "pallets?"}),
		step{text: text, hold: release},
	)
	trigger := a.mention(a.public, a.alice, "@assistant pallets?")
	runID := uuid.New()
	w := a.watch("ch:" + a.public.String())
	rowsBefore := a.outboxRows()

	done := make(chan error, 1)
	go func() { done <- a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)) }()

	// The model is held after its first piece: the frames seen now were streamed
	// while the run is still going.
	streaming := w.waitFor("a streamed piece", func(fs []*realtimev1.AgentRunProgress) bool {
		return slices.ContainsFunc(fs, func(p *realtimev1.AgentRunProgress) bool {
			return p.GetState() == realtimev1.AgentRunState_AGENT_RUN_STATE_STREAMING && p.GetTextDelta() != ""
		})
	})
	if got := a.outboxRows(); got != rowsBefore {
		t.Errorf("outbox rows while streaming = %d, want the %d it had before: progress is not outboxed", got, rowsBefore)
	}
	for _, p := range streaming {
		if p.GetRunId() != runID.String() {
			t.Errorf("frame %v names run %q, want %s", p, p.GetRunId(), runID)
		}
	}
	if got := a.run(runID).Status; got != "running" {
		t.Errorf("run status while streaming = %q, want running", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("agent.run: %v", err)
	}

	frames := w.waitFor("the DONE frame", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_DONE))
	var streamed strings.Builder
	for _, p := range frames {
		if p.GetState() == realtimev1.AgentRunState_AGENT_RUN_STATE_STREAMING {
			streamed.WriteString(p.GetTextDelta())
		}
	}
	if streamed.String() != text {
		t.Errorf("streamed text = %q, want the model's %q", streamed.String(), text)
	}
	last := frames[len(frames)-1]
	if last.GetState() != realtimev1.AgentRunState_AGENT_RUN_STATE_DONE || last.GetAnswer().GetRef() != "ANS-1001" ||
		last.GetAnswer().GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER {
		t.Errorf("last frame = %v, want DONE with the answer slip ANS-1001", last)
	}
	if got := a.outboxRows(); got != rowsBefore+1 {
		t.Errorf("outbox rows after the run = %d, want %d: the posted reply and nothing else", got, rowsBefore+1)
	}
	if n := a.count(`SELECT count(*) FROM platform.outbox WHERE topic <> 'chat.message.created'`); n != 0 {
		t.Errorf("%d outbox rows are not the reply's message event", n)
	}
}

func TestInvoke_FinalAttemptFailure_MarksRunFailed(t *testing.T) {
	a := newAgentFixture(t)
	logs := captureLogs(t)
	leaky := errors.New(`Post "http://litellm.internal.example:4000/v1/chat/completions": dial tcp: connection refused`)
	a.model.script(step{err: leaky}, step{err: leaky})
	trigger := a.mention(a.public, a.alice, "@assistant anything?")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)
	w := a.watch("ch:" + a.public.String())

	// An early failure is nacked, so the bus tries again; the run is not over.
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 4)); err == nil {
		t.Fatal("attempt 4 failed but was acked: nothing would deliver it again")
	}
	if got := a.run(runID).Status; got != "running" {
		t.Errorf("run status after attempt 4 = %q, want running: it has a delivery left", got)
	}
	if n := a.count(`SELECT count(*) FROM platform.processed_events WHERE consumer = 'agent.run'`); n != 0 {
		t.Errorf("processed_events rows = %d, want none: the redelivery starts over", n)
	}

	// The last attempt settles the run instead of leaving it running for ever.
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 5)); err != nil {
		t.Fatalf("attempt 5: %v, want the failure recorded and the message acked", err)
	}
	run := a.run(runID)
	if run.Status != "failed" || run.Ref != nil || len(run.Result) != 0 {
		t.Errorf("run = %+v, want failed with no REF and no answer: a failed run issues none", run)
	}
	if run.FailureReason == nil || *run.FailureReason != "model_unavailable" {
		t.Errorf("failure reason = %v, want the code model_unavailable, which does not repeat the provider's error", run.FailureReason)
	}
	if n := len(a.replies(a.public, a.alice)); n != 0 {
		t.Errorf("agent replies = %d, want none", n)
	}
	if n := a.count(`SELECT count(*) FROM platform.ref_counters WHERE kind = 'ANS'`); n != 0 {
		t.Errorf("ANS counters = %d, want none: a failed run issues no REF", n)
	}
	w.waitFor("the FAILED frame", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_FAILED))
	if got := logs.String(); !strings.Contains(got, "giving up on a run at the final attempt") || !strings.Contains(got, runID.String()) {
		t.Errorf("log = %q, want the give-up logged with the run id", got)
	}
	// Redelivered once more, the message is acked as already processed.
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 5)); err != nil {
		t.Errorf("redelivery after the failure was recorded: %v", err)
	}

	got, err := a.getRun(a.alice, runID)
	if err != nil || got.GetStatus() != agentsv1.RunStatus_RUN_STATUS_FAILED || got.GetFailureReason() == "" || got.GetRef() != "" || got.GetAnswer() != nil {
		t.Errorf("GetRun = %v, %v, want FAILED with its reason and no REF", got, err)
	}
}

func TestInvoke_NotFoundAnswer_PostsNotFoundArtifactWithAnsRefAndScope(t *testing.T) {
	a := newAgentFixture(t)
	// Carol's only channel holds nothing: the search completes and finds nothing.
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": "What is the parental leave policy?"}),
		say("I could not find that in your channels."),
	)
	trigger := a.mention(a.elsewhere, a.carol, "@assistant what is the parental leave policy?")
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}

	replies := a.replies(a.elsewhere, a.carol)
	if len(replies) != 1 {
		t.Fatalf("agent replies = %d, want 1", len(replies))
	}
	art := replies[0].GetArtifact()
	if art.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_NOT_FOUND || art.GetRef() != "ANS-1001" {
		t.Fatalf("artifact = %v, want NOT_FOUND with ANS-1001: a completed search that found nothing", art)
	}
	if len(art.GetCitations()) != 0 {
		t.Errorf("a not-found slip carries citations %v", art.GetCitations())
	}
	if !slices.Equal(art.GetScopeChannelIds(), idStrings(a.elsewhere)) {
		t.Errorf("scope_channel_ids = %v, want the channels that were searched: %v", art.GetScopeChannelIds(), a.elsewhere)
	}
	run := a.run(runID)
	if run.Status != "done" || run.Ref == nil || *run.Ref != "ANS-1001" || run.FailureReason != nil {
		t.Errorf("run = %+v, want DONE with ANS-1001, not failed", run)
	}
}

func TestInvoke_NoToolArtifact_PostsPlainAgentText(t *testing.T) {
	a := newAgentFixture(t)
	a.model.script(say("Happy to help! Ask me about your team's work."))
	trigger := a.mention(a.public, a.alice, "@assistant hello")
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}

	replies := a.replies(a.public, a.alice)
	if len(replies) != 1 || replies[0].GetBody() != "Happy to help! Ask me about your team's work." || replies[0].GetArtifact() != nil {
		t.Fatalf("replies = %v, want one plain agent message without a slip", replies)
	}
	if replies[0].GetAuthorId() != authz.AgentID("assistant").String() {
		t.Errorf("author_id = %s, want the agent's", replies[0].GetAuthorId())
	}
	run := a.run(runID)
	if run.Status != "done" || run.Ref != nil || len(run.Result) != 0 || string(run.ToolCalls) != "[]" {
		t.Errorf("run = %+v, want done with no REF, no slip and no tool calls: chatter has no seal", run)
	}
	if n := a.count(`SELECT count(*) FROM platform.ref_counters WHERE kind = 'ANS'`); n != 0 {
		t.Errorf("ANS counters = %d, want none", n)
	}
}

func TestInvoke_ChannelSummary_PostsChannelSummarySlipWithRef(t *testing.T) {
	a := newAgentFixture(t)
	a.say(a.public, a.alice, "Launch moves to Friday.")
	a.say(a.public, a.bob, "Agreed, QA signs off Thursday.")
	a.llm.HandleGenerate(func(context.Context, llm.Request) (llm.Result, error) {
		return llm.Result{Text: `{"summary":"The launch moves to Friday; QA signs off Thursday.","source_seqs":[1,2]}`}, nil
	})
	a.model.script(
		callTool("summarize_channel", map[string]any{"channel_id": a.public.String()}),
		say("Here is the catch-up."),
	)
	trigger := a.mention(a.public, a.alice, "@assistant catch me up")
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	replies := a.replies(a.public, a.alice)
	if len(replies) != 1 {
		t.Fatalf("agent replies = %d, want 1", len(replies))
	}
	art := replies[0].GetArtifact()
	if art.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_CHANNEL_SUMMARY || art.GetRef() != "ANS-1001" ||
		art.GetBodyMarkdown() != "The launch moves to Friday; QA signs off Thursday." || len(art.GetSources()) != 2 {
		t.Errorf("artifact = %v, want a CHANNEL_SUMMARY with ANS-1001, the summary and its two message sources", art)
	}
	for _, s := range art.GetSources() {
		if s.GetKind() != chatv1.SourceKind_SOURCE_KIND_MESSAGE || s.GetChannelId() != a.public.String() {
			t.Errorf("source = %v, want a message of the public channel", s)
		}
	}
}

func TestInvoke_CreateTaskDraft_CitesTheMentionThatStartedTheRun(t *testing.T) {
	a := newAgentFixture(t)
	trigger := a.mention(a.public, a.alice, "@assistant draft a task for Bob to book the dock")
	a.model.script(
		callTool("create_task_draft", map[string]any{
			"channel_id": a.public.String(), "title": "Book the dock", "owner_user_id": a.bob.String(), "due_hint": "+2d",
		}),
		say("I drafted it; Bob has to confirm it."),
	)
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	var state, createdBy, source string
	a.scan(&state, `SELECT state FROM tasks.tasks WHERE title = 'Book the dock'`)
	a.scan(&createdBy, `SELECT created_by_kind FROM tasks.tasks WHERE title = 'Book the dock'`)
	a.scan(&source, `SELECT source_id::text FROM tasks.tasks WHERE title = 'Book the dock'`)
	if state != "draft" || createdBy != "agent" || source != trigger.GetId() {
		t.Errorf("task state/created_by/source = %s/%s/%s, want a draft by the agent that cites the mention %s", state, createdBy, source, trigger.GetId())
	}
	// The model was told who Bob is: the mention carries the person's id.
	var seen strings.Builder
	for _, c := range a.model.requests()[0].Contents {
		for _, p := range c.Parts {
			seen.WriteString(p.Text)
		}
	}
	if !strings.Contains(seen.String(), a.public.String()) {
		t.Errorf("the model was not told the channel id: %q", seen.String())
	}
	if run := a.run(runID); run.Status != "done" || run.Ref != nil {
		t.Errorf("run = %+v, want done without a REF: a draft has the task's own REF, not an answer's", run)
	}
}

func TestInvoke_InvokerNoLongerInChannel_FailsTheRunWithoutAPost(t *testing.T) {
	a := newAgentFixture(t)
	logs := captureLogs(t)
	trigger := a.mention(a.public, a.alice, "@assistant hello")
	a.exec(`DELETE FROM chat.channel_members WHERE channel_id = $1 AND user_id = $2`, a.public, a.alice)
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v, want the permanent failure recorded and the message acked", err)
	}
	if run := a.run(runID); run.Status != "failed" || run.FailureReason == nil {
		t.Errorf("run = %+v, want failed with a reason", run)
	}
	if !strings.Contains(logs.String(), "agents: run cannot start") {
		t.Errorf("log = %q, want the permanent failure logged", logs.String())
	}
	if n := len(a.model.requests()); n != 0 {
		t.Errorf("model calls = %d, want none: a person who left the channel is not served", n)
	}
	if n := a.count(`SELECT count(*) FROM chat.messages WHERE author_kind = 'agent'`); n != 0 {
		t.Errorf("agent messages = %d, want none", n)
	}
}

func TestInvoke_UnknownAgent_FailsTheRunWithoutAskingAModel(t *testing.T) {
	a := newAgentFixture(t)
	trigger := a.mention(a.public, a.alice, "@assistant hello")
	ev := a.invokeOf(trigger, uuid.New())
	ev.Agent = "nobody"

	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v, want the permanent failure recorded and the message acked", err)
	}
	runID := uuid.MustParse(ev.GetRunId())
	if run := a.run(runID); run.Status != "failed" || run.FailureReason == nil {
		t.Errorf("run = %+v, want failed with a reason", run)
	}
	if n := len(a.model.requests()); n != 0 {
		t.Errorf("model calls = %d, want none", n)
	}
}

// ---- the runtime seam ----------------------------------------------------------

type stubRuntime struct {
	mu     sync.Mutex
	spec   Spec
	input  string
	sess   Session
	result Final
	err    error
}

func (r *stubRuntime) Run(_ context.Context, spec Spec, input string, sess Session, emit func(string)) (Final, error) {
	r.mu.Lock()
	r.spec, r.input, r.sess = spec, input, sess
	r.mu.Unlock()
	emit("par")
	emit("tial")
	return r.result, r.err
}

func TestInvoke_RuntimeIsReplaceable(t *testing.T) {
	a := newAgentFixture(t)
	rt := &stubRuntime{result: Final{Text: "from the stub", Usage: llm.Usage{InputTokens: 7, OutputTokens: 3}}}
	a.deps.Runtime = rt
	trigger := a.mention(a.public, a.alice, "@assistant hi")
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	if replies := a.replies(a.public, a.alice); len(replies) != 1 || replies[0].GetBody() != "from the stub" {
		t.Errorf("replies = %v, want the stub's text", replies)
	}
	if run := a.run(runID); run.InputTokens != 7 || run.OutputTokens != 3 {
		t.Errorf("tokens = %d/%d, want the stub's 7/3", run.InputTokens, run.OutputTokens)
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.spec.Name != "assistant" || rt.sess.UserID != a.alice.String() || !strings.HasPrefix(rt.sess.ID, "assistant:"+a.alice.String()+":") ||
		!strings.HasSuffix(rt.sess.ID, a.public.String()) {
		t.Errorf("runtime got spec %q and session %+v, want the assistant in the session of (alice, channel)", rt.spec.Name, rt.sess)
	}
	if len(rt.sess.Tools) != 5 || rt.sess.Service == nil || !strings.Contains(rt.input, "hi") {
		t.Errorf("runtime session has %d tools, input %q", len(rt.sess.Tools), rt.input)
	}
}

// ---- sessions ----------------------------------------------------------------

func TestSession_PersistsAcrossRunnerRestart(t *testing.T) {
	a := newAgentFixture(t)
	a.model.script(say("Noted: Thursday."), say("You said Thursday."))
	first := a.mention(a.public, a.alice, "@assistant remember that the review is on Thursday")

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(first, uuid.New()), "evt-1", 1)); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A new process: new jobs, new runner, the same database.
	second := a.mention(a.public, a.alice, "@assistant when is the review?")
	if err := a.job()(t.Context(), a.delivery(a.invokeOf(second, uuid.New()), "evt-2", 1)); err != nil {
		t.Fatalf("second run: %v", err)
	}

	history := func(req *model.LLMRequest) string {
		var b strings.Builder
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				b.WriteString(string(c.Role) + ": " + p.Text + "\n")
			}
		}
		return b.String()
	}
	reqs := a.model.requests()
	if len(reqs) != 2 {
		t.Fatalf("model calls = %d, want 2", len(reqs))
	}
	got := history(reqs[1])
	for _, want := range []string{"review is on Thursday", "Noted: Thursday.", "when is the review?"} {
		if !strings.Contains(got, want) {
			t.Errorf("the second run's prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(history(reqs[0]), "when is the review?") {
		t.Errorf("the first run's prompt already holds the second question")
	}

	// One session per (user, channel): another channel or another person starts clean.
	a.model.script(say("A fresh start."))
	other := a.mention(a.public, a.bob, "@assistant hi from bob")
	if err := a.job()(t.Context(), a.delivery(a.invokeOf(other, uuid.New()), "evt-3", 1)); err != nil {
		t.Fatalf("bob's run: %v", err)
	}
	if h := history(a.model.requests()[2]); strings.Contains(h, "Thursday") {
		t.Errorf("bob's prompt holds alice's conversation:\n%s", h)
	}
	if n := a.count(`SELECT count(*) FROM agents.sessions`); n != 2 {
		t.Errorf("sessions = %d, want 2: one for alice and one for bob in the channel", n)
	}
}

// ---- Ask panel ---------------------------------------------------------------

func (a *agentFixture) getRun(as, id uuid.UUID) (*agentsv1.Run, error) {
	return New(a.deps).GetRun(a.t.Context(), a.person(as), id)
}

func TestAskPanelRun_ProgressOnUserTopicAndResultViaGetRun(t *testing.T) {
	a := newAgentFixture(t)
	a.seedWarehouse()
	a.model.script(
		callTool("search_knowledge", map[string]any{"question": "How many pallets does the warehouse hold?"}),
		say("The warehouse holds 4,000 pallets [1]."),
	)
	svc := New(a.deps)
	w := a.watch("u:" + a.alice.String())
	wCh := a.watch("ch:" + a.public.String())

	run, err := svc.StartRun(t.Context(), a.person(a.alice), &agentsv1.StartRunRequest{Agent: "assistant", Question: "  How many pallets does the warehouse hold?  "})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	runID := uuid.MustParse(run.GetId())
	if run.GetStatus() != agentsv1.RunStatus_RUN_STATUS_QUEUED || run.GetAgent() != "assistant" || run.GetQuestion() != "How many pallets does the warehouse hold?" || run.GetChannelId() != "" {
		t.Errorf("StartRun = %v, want a queued run of the assistant with the trimmed question and no channel", run)
	}
	// It queued exactly one agent.invoke event, in the transaction of the run row.
	var topic, payload string
	a.scan(&topic, `SELECT topic FROM platform.outbox WHERE topic = 'agent.invoke'`)
	a.scan(&payload, `SELECT payload::text FROM platform.outbox WHERE topic = 'agent.invoke'`)
	var ev eventsv1.AgentInvoke
	if err := (protojson.UnmarshalOptions{}).Unmarshal([]byte(payload), &ev); err != nil {
		t.Fatalf("outbox payload %s: %v", payload, err)
	}
	if ev.GetWorkspaceId() != a.ws.String() || ev.GetRunId() != run.GetId() || ev.GetAgent() != "assistant" ||
		ev.GetOnBehalfOf() != a.alice.String() || ev.GetChannelId() != "" || ev.GetTriggerMessageId() != "" {
		t.Errorf("agent.invoke = %v, want the run of alice's Ask-panel question", &ev)
	}
	if got, err := svc.GetRun(t.Context(), a.person(a.alice), runID); err != nil || got.GetStatus() != agentsv1.RunStatus_RUN_STATUS_QUEUED {
		t.Fatalf("GetRun before the run = %v, %v, want queued", got, err)
	}

	// The relay delivers the row's message.
	msg := a.delivery(&ev, "evt-ask", 1)
	if err := a.job()(t.Context(), msg); err != nil {
		t.Fatalf("agent.run: %v", err)
	}

	frames := w.waitFor("the DONE frame on the user topic", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_DONE))
	if !hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_STREAMING)(frames) {
		t.Errorf("frames on u:{alice} = %v, want streamed text before DONE", frames)
	}
	if got := wCh.snapshot(); len(got) != 0 {
		t.Errorf("frames on the channel topic = %v, want none: an Ask-panel run is private to its user", got)
	}
	if n := len(a.replies(a.public, a.alice)); n != 0 {
		t.Errorf("agent replies in the channel = %d, want none: the result is for GetRun", n)
	}

	got, err := svc.GetRun(t.Context(), a.person(a.alice), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.GetStatus() != agentsv1.RunStatus_RUN_STATUS_DONE || got.GetRef() != "ANS-1001" || got.GetAnswer().GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER ||
		got.GetAnswer().GetRef() != "ANS-1001" || len(got.GetAnswer().GetCitations()) == 0 || got.GetAnswer().GetBodyMarkdown() == "" {
		t.Errorf("GetRun = %v, want DONE with the cited ANSWER slip ANS-1001", got)
	}
	last := frames[len(frames)-1]
	if last.GetState() != realtimev1.AgentRunState_AGENT_RUN_STATE_DONE || last.GetAnswer().GetRef() != "ANS-1001" {
		t.Errorf("last frame = %v, want DONE with the slip", last)
	}
}

// startAsk asks the Ask panel as alice and returns the run with the event the relay
// delivers for it.
func (a *agentFixture) startAsk(svc Service, question string) (*agentsv1.Run, *eventsv1.AgentInvoke) {
	a.t.Helper()
	run, err := svc.StartRun(a.t.Context(), a.person(a.alice), &agentsv1.StartRunRequest{Agent: "assistant", Question: question})
	if err != nil {
		a.t.Fatalf("StartRun: %v", err)
	}
	return run, a.queuedInvoke(run)
}

func TestAskPanelRun_DraftToolHasNoMessageToCite(t *testing.T) {
	a := newAgentFixture(t)
	a.model.script(
		callTool("create_task_draft", map[string]any{"channel_id": a.public.String(), "title": "Book the dock"}),
		say("I can only draft a task from a message in a channel."),
	)
	svc := New(a.deps)
	run, ev := a.startAsk(svc, "draft a task to book the dock")

	if err := a.job()(t.Context(), a.delivery(ev, "evt-ask", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	id := uuid.MustParse(run.GetId())
	row := a.run(id)
	if row.Status != "done" {
		t.Fatalf("run = %+v, want done: a tool that refuses is an answer for the model, not a failure of the run", row)
	}
	calls := a.recordedCalls(id)
	if len(calls) != 2 || calls[0].Name != "create_task_draft" || calls[0].Result != "" || !strings.Contains(calls[0].Error, "invalid") ||
		calls[1].Name != "search_knowledge" || !calls[1].Forced {
		t.Errorf("tool_calls = %+v, want the refused call recorded with its error, then the search the program made", calls)
	}
	// The model had nothing to source its text with, so the Ask is answered by the search, and its text is not.
	if slip := a.answerOf(svc, id); slip.GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_NOT_FOUND || strings.Contains(slip.String(), "only draft") {
		t.Errorf("slip = %v, want the NOT_FOUND slip of the program's search and nothing the model said", slip)
	}
	if n := a.count(`SELECT count(*) FROM tasks.tasks`); n != 0 {
		t.Errorf("tasks = %d, want none", n)
	}
}

func TestInvoke_EmptyAnswerIsRetriedAndNeverPosted(t *testing.T) {
	a := newAgentFixture(t)
	logs := captureLogs(t)
	a.deps.Runtime = &stubRuntime{}
	trigger := a.mention(a.public, a.alice, "@assistant hi")
	runID := uuid.New()
	ev := a.invokeOf(trigger, runID)

	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", 1)); err == nil {
		t.Fatal("a run that said nothing was acked: nothing would deliver it again")
	}
	if n := len(a.replies(a.public, a.alice)); n != 0 {
		t.Errorf("agent replies = %d, want none: a message cannot be empty", n)
	}
	if err := a.job()(t.Context(), a.delivery(ev, "evt-1", defaultFinalAttempt)); err != nil {
		t.Fatalf("final attempt: %v", err)
	}
	if run := a.run(runID); run.Status != "failed" || run.FailureReason == nil {
		t.Errorf("run = %+v, want failed with a reason", run)
	}
	if !strings.Contains(logs.String(), "giving up on a run") {
		t.Errorf("log = %q, want the give-up logged", logs.String())
	}
}

func TestInvoke_LongReplyIsCutToTheMessageLimit(t *testing.T) {
	a := newAgentFixture(t)
	a.deps.Runtime = &stubRuntime{result: Final{Text: strings.Repeat("é", maxBodyRunes+500)}}
	trigger := a.mention(a.public, a.alice, "@assistant tell me everything")
	runID := uuid.New()

	if err := a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-1", 1)); err != nil {
		t.Fatalf("agent.run: %v", err)
	}
	replies := a.replies(a.public, a.alice)
	if len(replies) != 1 || utf8.RuneCountInString(replies[0].GetBody()) != maxBodyRunes || !strings.HasSuffix(replies[0].GetBody(), "…") {
		t.Fatalf("replies = %d, body of %d runes, want one reply of exactly %d runes cut with …", len(replies), utf8.RuneCountInString(firstBody(replies)), maxBodyRunes)
	}
	if run := a.run(runID); run.Status != "done" {
		t.Errorf("run = %+v, want done", run)
	}
}

func firstBody(msgs []*chatv1.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[0].GetBody()
}

func TestNew_PanicsWhenADependencyIsMissing(t *testing.T) {
	a := newAgentFixture(t)
	for name, drop := range map[string]func(*Deps){
		"DB":        func(d *Deps) { d.DB = nil },
		"Authz":     func(d *Deps) { d.Authz = nil },
		"Chat":      func(d *Deps) { d.Chat = nil },
		"Knowledge": func(d *Deps) { d.Knowledge = nil },
		"Tasks":     func(d *Deps) { d.Tasks = nil },
		"LLM":       func(d *Deps) { d.LLM = nil },
		"Frames":    func(d *Deps) { d.Frames = nil },
	} {
		t.Run(name, func(t *testing.T) {
			d := a.deps
			drop(&d)
			defer func() {
				if msg, _ := recover().(string); !strings.Contains(msg, "Deps."+name) {
					t.Errorf("panic = %q, want it to name Deps.%s", msg, name)
				}
			}()
			New(d)
		})
	}
}

func TestStartRun_ChecksTheRequestAndTheChannel(t *testing.T) {
	a := newAgentFixture(t)
	svc := New(a.deps)
	alice := a.person(a.alice)
	ok := &agentsv1.StartRunRequest{Agent: "assistant", Question: "anything?"}
	for name, tc := range map[string]struct {
		actor authz.Actor
		req   *agentsv1.StartRunRequest
		want  error
	}{
		"unknown agent":   {alice, &agentsv1.StartRunRequest{Agent: "nobody", Question: "x"}, errs.ErrInvalid},
		"no agent":        {alice, &agentsv1.StartRunRequest{Question: "x"}, errs.ErrInvalid},
		"blank question":  {alice, &agentsv1.StartRunRequest{Agent: "assistant", Question: " \n "}, errs.ErrInvalid},
		"huge question":   {alice, &agentsv1.StartRunRequest{Agent: "assistant", Question: strings.Repeat("x", maxQuestionRunes+1)}, errs.ErrInvalid},
		"bad channel id":  {alice, &agentsv1.StartRunRequest{Agent: "assistant", Question: "x", ChannelId: proto.String("not-a-uuid")}, errs.ErrInvalid},
		"not a member":    {alice, &agentsv1.StartRunRequest{Agent: "assistant", Question: "x", ChannelId: proto.String(a.private.String())}, errs.ErrForbidden},
		"an agent actor":  {a.agentFor(a.alice), ok, errs.ErrForbidden},
		"no actor at all": {authz.Actor{}, ok, errs.ErrForbidden},
		"a system actor":  {authz.Actor{Kind: authz.ActorSystem, WorkspaceID: a.ws, UserID: a.alice}, ok, errs.ErrForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.StartRun(t.Context(), tc.actor, tc.req); !errors.Is(err, tc.want) {
				t.Errorf("StartRun = %v, want %v", err, tc.want)
			}
		})
	}
	if n := a.count(`SELECT count(*) FROM agents.runs`) + a.outboxRows(); n != 0 {
		t.Errorf("refused requests left %d rows behind", n)
	}

	run, err := svc.StartRun(t.Context(), alice, &agentsv1.StartRunRequest{Agent: "assistant", Question: "q", ChannelId: proto.String(a.public.String())})
	if err != nil || run.GetChannelId() != a.public.String() {
		t.Fatalf("StartRun in a channel of alice = %v, %v", run, err)
	}
	var payload string
	a.scan(&payload, `SELECT payload::text FROM platform.outbox WHERE topic = 'agent.invoke'`)
	var ev eventsv1.AgentInvoke
	if err := protojson.Unmarshal([]byte(payload), &ev); err != nil || ev.GetChannelId() != a.public.String() || ev.GetTriggerMessageId() != "" {
		t.Errorf("agent.invoke = %v (%v), want the channel and no trigger message: still an Ask-panel run", &ev, err)
	}
}

func TestGetRun_OtherUserForbidden(t *testing.T) {
	a := newAgentFixture(t)
	svc := New(a.deps)
	run, err := svc.StartRun(t.Context(), a.person(a.alice), &agentsv1.StartRunRequest{Agent: "assistant", Question: "secret question"})
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.MustParse(run.GetId())

	if _, err := svc.GetRun(t.Context(), a.person(a.bob), id); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("GetRun as another member = %v, want ErrForbidden", err)
	}
	if _, err := svc.GetRun(t.Context(), a.agentFor(a.alice), id); !errors.Is(err, errs.ErrForbidden) {
		t.Errorf("GetRun as an agent = %v, want ErrForbidden: only the person who asked reads the run", err)
	}
	if _, err := svc.GetRun(t.Context(), a.person(a.alice), uuid.New()); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("GetRun of an unknown run = %v, want ErrNotFound", err)
	}
	if got, err := svc.GetRun(t.Context(), a.person(a.alice), id); err != nil || got.GetQuestion() != "secret question" {
		t.Errorf("GetRun as the owner = %v, %v", got, err)
	}
}

// ---- Connect -----------------------------------------------------------------

func TestConnect_RequiresTheProtocolHeader(t *testing.T) {
	a := newAgentFixture(t)
	actor := a.person(a.alice)
	// No option is passed: the handler adds the requirement itself.
	path, h := Handler(New(a.deps), func(context.Context) authz.Actor { return actor })
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	call := func(version string) (int, string) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path+"StartRun",
			strings.NewReader(fmt.Sprintf(`{"agent":"assistant","question":"over Connect %s"}`, version)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if version != "" {
			req.Header.Set("Connect-Protocol-Version", version)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	status, body := call("")
	if status != http.StatusBadRequest || !strings.Contains(body, "Connect-Protocol-Version") {
		t.Fatalf("POST without the header: %d %s; want 400 naming Connect-Protocol-Version", status, body)
	}
	if n := a.count(`SELECT count(*) FROM agents.runs`); n != 0 {
		t.Fatalf("the refused call reached the service: %d runs recorded", n)
	}
	status, body = call("1")
	if status != http.StatusOK || !strings.Contains(body, "RUN_STATUS_QUEUED") {
		t.Fatalf("POST with the header: %d %s; want 200 and a queued run", status, body)
	}

	// The generated client sends the header itself and reaches both methods.
	client := agentsv1connect.NewAgentServiceClient(srv.Client(), srv.URL)
	started, err := client.StartRun(t.Context(), connect.NewRequest(&agentsv1.StartRunRequest{Agent: "assistant", Question: "again"}))
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	got, err := client.GetRun(t.Context(), connect.NewRequest(&agentsv1.GetRunRequest{RunId: started.Msg.GetRun().GetId()}))
	if err != nil || got.Msg.GetRun().GetQuestion() != "again" {
		t.Fatalf("GetRun = %v, %v", got, err)
	}
	if _, err := client.GetRun(t.Context(), connect.NewRequest(&agentsv1.GetRunRequest{RunId: "not-a-uuid"})); err == nil {
		t.Error("GetRun with a malformed id succeeded")
	}
}

// ---- the session service -----------------------------------------------------

const testApp = "kiwi"

// turn is an event of author with text, as the runner appends them.
func turn(t *testing.T, author, text string) *session.Event {
	t.Helper()
	ev := session.NewEvent(t.Context(), "inv-1")
	ev.Author = author
	var role genai.Role = genai.RoleModel
	if author == "user" {
		role = genai.RoleUser
	}
	ev.Content = genai.NewContentFromText(text, role)
	return ev
}

func TestSessionService_StoresEventsAndStateAndReloadsThem(t *testing.T) {
	f := newFixture(t)
	svc := newSessionService(f.db, f.ws)
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "s-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sess := created.Session

	user := turn(t, "user", "what is on the board?")
	call := session.NewEvent(t.Context(), "inv-1")
	call.Author = "assistant"
	call.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "list_tasks", Args: map[string]any{"channel_id": "x"}}}}}
	reply := session.NewEvent(t.Context(), "inv-1")
	reply.Author = "tool"
	reply.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "c1", Name: "list_tasks", Response: map[string]any{"total": float64(2)}}}}}
	final := turn(t, "assistant", "Two tasks.")
	final.Actions.StateDelta = map[string]any{"topic": "board", "temp:scratch": "never stored"}
	partial := turn(t, "assistant", "Two")
	partial.Partial = true
	noID := turn(t, "assistant", "Anything else?")
	noID.ID = ""

	for _, ev := range []*session.Event{user, call, reply, partial, final, noID} {
		if err := svc.AppendEvent(t.Context(), sess, ev); err != nil {
			t.Fatalf("AppendEvent(%s): %v", ev.Author, err)
		}
	}
	if noID.ID == "" {
		t.Error("an event without an id was stored without one: the service must name it, in place")
	}
	if n := sess.Events().Len(); n != 5 {
		t.Errorf("live session holds %d events, want 5: the partial one is not part of the conversation", n)
	}
	if v, err := sess.State().Get("topic"); err != nil || v != "board" {
		t.Errorf("live state topic = %v, %v, want board", v, err)
	}

	// A new service over the same database, as after a restart.
	got, err := newSessionService(f.db, f.ws).Get(t.Context(), &session.GetRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "s-1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	loaded := got.Session
	if loaded.ID() != "s-1" || loaded.AppName() != testApp || loaded.UserID() != f.alice.String() {
		t.Errorf("loaded session = %s/%s/%s", loaded.ID(), loaded.AppName(), loaded.UserID())
	}
	var authors []string
	for ev := range loaded.Events().All() {
		authors = append(authors, ev.Author)
	}
	if want := []string{"user", "assistant", "tool", "assistant", "assistant"}; !slices.Equal(authors, want) {
		t.Errorf("event authors = %v, want %v in append order", authors, want)
	}
	if fc := loaded.Events().At(1).Content.Parts[0].FunctionCall; fc == nil || fc.Name != "list_tasks" || fc.Args["channel_id"] != "x" {
		t.Errorf("stored function call = %+v, want the one that was appended", fc)
	}
	if fr := loaded.Events().At(2).Content.Parts[0].FunctionResponse; fr == nil || fr.Response["total"] != float64(2) {
		t.Errorf("stored function response = %+v", fr)
	}
	if loaded.Events().At(4).ID == "" || loaded.Events().At(0).Timestamp.IsZero() {
		t.Errorf("stored events lost their id or timestamp: %+v", loaded.Events().At(4))
	}
	if v, err := loaded.State().Get("topic"); err != nil || v != "board" {
		t.Errorf("loaded state topic = %v, %v, want board", v, err)
	}
	if _, err := loaded.State().Get("temp:scratch"); !errors.Is(err, session.ErrStateKeyNotExist) {
		t.Errorf("a temp: key was stored: %v", err)
	}
	if loaded.LastUpdateTime().IsZero() {
		t.Error("LastUpdateTime is zero")
	}

	// Create again is not an error: two runs of one conversation may race to start it.
	if again, err := svc.Create(t.Context(), &session.CreateRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "s-1"}); err != nil || again.Session.Events().Len() != 5 {
		t.Errorf("second Create = %v, %v, want the session that is there", again, err)
	}
	if err := svc.AppendEvent(t.Context(), &stranger{sess}, turn(t, "user", "x")); err == nil {
		t.Error("AppendEvent accepted a session this service did not make")
	}
}

// stranger is a session of another implementation.
type stranger struct{ session.Session }

func TestSessionService_SessionsBelongToTheirUserAndWorkspace(t *testing.T) {
	f := newFixture(t)
	svc := newSessionService(f.db, f.ws)
	if _, err := svc.Create(t.Context(), &session.CreateRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "alice-s"}); err != nil {
		t.Fatal(err)
	}
	get := func(s session.Service, app, user, id string) error {
		_, err := s.Get(t.Context(), &session.GetRequest{AppName: app, UserID: user, SessionID: id})
		return err
	}
	if err := get(svc, testApp, f.alice.String(), "alice-s"); err != nil {
		t.Fatalf("Get by its owner: %v", err)
	}
	if err := get(svc, testApp, f.bob.String(), "alice-s"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Get by another user = %v, want ErrNotFound: a session is never loaded for anyone but its user", err)
	}
	if err := get(svc, "other-app", f.alice.String(), "alice-s"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Get under another app = %v, want ErrNotFound", err)
	}
	if err := get(svc, testApp, f.alice.String(), "missing"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Get of a missing session = %v, want ErrNotFound", err)
	}
	if err := get(svc, testApp, "not-a-uuid", "alice-s"); err == nil || errors.Is(err, session.ErrNotFound) {
		t.Errorf("Get for a malformed user = %v, want a plain error", err)
	}

	// Another workspace, through row-level security, sees nothing of it.
	other := f.newWorkspace()
	if err := get(newSessionService(f.db, other), testApp, f.alice.String(), "alice-s"); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("Get from another workspace = %v, want ErrNotFound", err)
	}

	ghost := &session.CreateRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "ghost"}
	made, err := svc.Create(t.Context(), ghost)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(t.Context(), &session.DeleteRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "ghost"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AppendEvent(t.Context(), made.Session, turn(t, "user", "hello?")); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("AppendEvent to a deleted session = %v, want ErrNotFound", err)
	}
	if err := svc.Delete(t.Context(), &session.DeleteRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "ghost"}); err != nil {
		t.Errorf("Delete of a session that is not there = %v, want nil", err)
	}
	list, err := svc.List(t.Context(), &session.ListRequest{AppName: testApp, UserID: f.alice.String()})
	if err != nil || len(list.Sessions) != 1 || list.Sessions[0].ID() != "alice-s" {
		t.Errorf("List = %v, %v, want only alice-s", list, err)
	}
	if list, err := svc.List(t.Context(), &session.ListRequest{AppName: testApp}); err != nil || len(list.Sessions) != 1 {
		t.Errorf("List of every user = %v, %v, want 1", list, err)
	}
}

func TestSessionService_LoadsOnlyTheRecentTurnsFromWhereATurnBegins(t *testing.T) {
	f := newFixture(t)
	svc := newSessionService(f.db, f.ws)
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "long"})
	if err != nil {
		t.Fatal(err)
	}
	// Turns of three events: the question, a tool call and its response, then the answer.
	turns := maxHistoryEvents // far more events than are loaded
	for i := range turns {
		q := turn(t, "user", fmt.Sprintf("question %d", i))
		call := session.NewEvent(t.Context(), "inv")
		call.Author = "assistant"
		call.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "c", Name: "t"}}}}
		resp := session.NewEvent(t.Context(), "inv")
		resp.Author = "assistant"
		resp.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "c", Name: "t", Response: map[string]any{}}}}}
		a := turn(t, "assistant", fmt.Sprintf("answer %d", i))
		for _, ev := range []*session.Event{q, call, resp, a} {
			if err := svc.AppendEvent(t.Context(), created.Session, ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := svc.Get(t.Context(), &session.GetRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "long"})
	if err != nil {
		t.Fatal(err)
	}
	events := got.Session.Events()
	if events.Len() == 0 || events.Len() > maxHistoryEvents {
		t.Fatalf("loaded %d events, want between 1 and %d", events.Len(), maxHistoryEvents)
	}
	if first := events.At(0); first.Author != "user" || first.Content.Parts[0].Text == "" {
		t.Errorf("the history begins with %s %+v, want a user turn: a tool response without its call is refused by providers", first.Author, first.Content)
	}
	if last := events.At(events.Len() - 1); last.Content.Parts[0].Text != fmt.Sprintf("answer %d", turns-1) {
		t.Errorf("the history ends with %q, want the newest answer", last.Content.Parts[0].Text)
	}

	// NumRecentEvents and After narrow it further.
	few, err := svc.Get(t.Context(), &session.GetRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "long", NumRecentEvents: 2})
	if err != nil || few.Session.Events().Len() != 2 {
		t.Errorf("Get with NumRecentEvents 2 = %v, %v, want 2 events", few, err)
	}
	none, err := svc.Get(t.Context(), &session.GetRequest{AppName: testApp, UserID: f.alice.String(), SessionID: "long", After: time.Now().Add(time.Hour)})
	if err != nil || none.Session.Events().Len() != 0 {
		t.Errorf("Get After the future = %v, %v, want no events", none, err)
	}
}

// ---- live -----------------------------------------------------------------------

// factualAnswer is a fake knowledge model that answers from the first chunk it is
// shown with a sentence that quotes it.
func factualAnswer(_ context.Context, req llm.Request) (llm.Result, error) {
	m := chunkTag.FindStringSubmatch(req.Parts[0].Text)
	if m == nil {
		return llm.Result{}, errors.New("no chunk in the prompt")
	}
	raw, err := json.Marshal(map[string]any{
		"answer_markdown": "The Bang Na warehouse holds 4,000 pallets. [1]",
		"citations":       []map[string]string{{"chunk_id": m[1], "quote": "4,000 pallets"}},
	})
	return llm.Result{Text: string(raw)}, err
}

// TestLive_MentionRun_ToolCallRoundTripAgainstLiteLLM is the real-condition check of the
// runtime: a mention run whose agent model is the pro tier of the live LiteLLM calls
// search_knowledge, reads the answer and replies. Everything behind the tool (the
// index, the embedding, the model that words the answer) is the scripted fake; the
// agent loop is real. It is opt-in: with the LiteLLM variables of .env loaded,
//
//	KIWI_LIVE_E2E=1 go test ./internal/agents -run TestLive -v
//
// The error of the model path may name the endpoint, so the test never prints one raw.
func TestLive_MentionRun_ToolCallRoundTripAgainstLiteLLM(t *testing.T) {
	if os.Getenv("KIWI_LIVE_E2E") != "1" {
		t.Skip("set KIWI_LIVE_E2E=1 (with the KIWI_LLM_* variables of .env) to run against the live LiteLLM")
	}
	cfg := config.Config{Env: "local", LLMProvider: "openai", EmbedBaseURL: "http://127.0.0.1:1", EmbedModel: "unused"}
	for env, dst := range map[string]*string{
		"KIWI_LLM_BASE_URL": &cfg.LLMBaseURL, "KIWI_LLM_API_KEY": &cfg.LLMAPIKey,
		"KIWI_LLM_MODEL_FLASH": &cfg.LLMModelFlash, "KIWI_LLM_MODEL_PRO": &cfg.LLMModelPro,
	} {
		if *dst = os.Getenv(env); *dst == "" {
			t.Fatalf("%s is not set", env)
		}
	}
	endpoint, err := url.Parse(cfg.LLMBaseURL)
	if err != nil {
		t.Fatal("KIWI_LLM_BASE_URL is not a URL")
	}
	scrub := func(s string) string {
		for _, h := range []string{endpoint.Host, endpoint.Hostname(), cfg.LLMBaseURL, cfg.LLMAPIKey} {
			if h != "" {
				s = strings.ReplaceAll(s, h, "<redacted>")
			}
		}
		return s
	}
	live, err := openai.New(cfg, nil)
	if err != nil {
		t.Fatal(scrub(err.Error()))
	}

	a := newAgentFixture(t)
	logs := captureLogs(t)
	a.llm.HandleADKModel(func(llm.Tier) (model.LLM, error) { return live.ADKModel(llm.TierPro) })
	a.llm.HandleGenerate(factualAnswer)
	a.indexedMeeting(a.public, a.bob, "Warehouse review", warehouseFact)
	w := a.watch("ch:" + a.public.String())
	trigger := a.mention(a.public, a.alice, "@assistant How many pallets does the Bang Na warehouse hold?")
	runID := uuid.New()

	start := time.Now()
	err = a.job()(t.Context(), a.delivery(a.invokeOf(trigger, runID), "evt-live", 1))
	took := time.Since(start)
	if err != nil {
		t.Fatalf("agent.run: %s\nlog: %s", scrub(err.Error()), scrub(logs.String()))
	}
	run := a.run(runID)
	if run.Status != "done" {
		t.Fatalf("run = %s, reason %v\nlog: %s", run.Status, run.FailureReason, scrub(logs.String()))
	}
	replies := a.replies(a.public, a.alice)
	if len(replies) != 1 {
		t.Fatalf("agent replies = %d, want 1", len(replies))
	}
	reply := replies[0]
	t.Logf("pro tier, %s: %d tool call(s), tokens %d in / %d out", took.Round(100*time.Millisecond), countCalls(t, run.ToolCalls), run.InputTokens, run.OutputTokens)
	t.Logf("tool_calls: %s", run.ToolCalls)
	t.Logf("reply: %q", reply.GetBody())
	t.Logf("slip: kind %s ref %s citations %d scope %v", reply.GetArtifact().GetKind(), reply.GetArtifact().GetRef(), len(reply.GetArtifact().GetCitations()), reply.GetArtifact().GetScopeChannelIds())

	if reply.GetBody() == "" || reply.GetArtifact().GetKind() != chatv1.ArtifactKind_ARTIFACT_KIND_ANSWER || reply.GetArtifact().GetRef() != "ANS-1001" {
		t.Errorf("reply = %v, want text and an ANSWER slip ANS-1001: the model must have called search_knowledge", reply)
	}
	if countCalls(t, run.ToolCalls) == 0 || run.InputTokens <= 0 || run.OutputTokens <= 0 {
		t.Errorf("tool calls %s, tokens %d/%d, want a call and counted tokens", run.ToolCalls, run.InputTokens, run.OutputTokens)
	}
	frames := w.waitFor("the DONE frame", hasState(realtimev1.AgentRunState_AGENT_RUN_STATE_DONE))
	streamed := 0
	for _, p := range frames {
		if p.GetState() == realtimev1.AgentRunState_AGENT_RUN_STATE_STREAMING {
			streamed++
		}
	}
	t.Logf("progress frames: %d total, %d streaming", len(frames), streamed)
	if streamed == 0 {
		t.Error("no streamed text frame reached Valkey")
	}
}

func countCalls(t *testing.T, raw []byte) int {
	t.Helper()
	var calls []ToolCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		t.Fatalf("tool_calls %s: %v", raw, err)
	}
	return len(calls)
}
