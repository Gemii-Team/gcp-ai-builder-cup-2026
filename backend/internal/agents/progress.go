package agents

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	chatv1 "kiwi/gen/kiwi/chat/v1"
	realtimev1 "kiwi/gen/kiwi/realtime/v1"
	"kiwi/internal/platform/outbox"
)

const (
	// frameTimeout bounds one publish of a progress frame: progress is a hint and a
	// Valkey that does not answer must not hold the run it describes.
	frameTimeout = 2 * time.Second
	// doneTimeout bounds the publish of the last frame of a run, which a person waits
	// for.
	doneTimeout = 5 * time.Second
)

// progress sends the frames of one run to the topic its watchers follow: the channel
// of a mention run, the user of an Ask-panel run. The frames are ephemeral: they go
// over Valkey and never through the outbox, so a frame that is lost is a delay and
// the run's stored state is the truth (a channel's message, GetRun).
//
// The contract a client reads the frames by:
//
//   - A run sends RUNNING when it starts and again after every tool call it made, then
//     STREAMING frames whose text_delta pieces, in order, are the text the model is
//     writing, then DONE or FAILED.
//   - The text a model writes between tool calls is not necessarily part of its answer
//     (it may say "let me look" before it searches), and the frames carry no marker for
//     where the final answer begins. So a client keeps one buffer per run and clears it
//     on every RUNNING, DONE and FAILED frame, and when the run's message arrives as a
//     message_created frame; whatever STREAMING text follows the last RUNNING is the
//     answer so far.
//   - An Ask-panel run streams no text until a tool has left a result to build its slip
//     from: before that the model may be answering from the conversation, which is
//     never an Ask's answer.
//   - DONE carries the slip when the run produced one, and nothing else of the answer.
//     Every Ask-panel run ends with a slip (the search the program makes itself when the
//     model made none: see execute), so its answer is that slip; a reply without a slip is
//     the plain message a channel mention posts. A frame may be lost or arrive twice;
//     DONE and FAILED are repeated by nobody, so a client that is waiting for a run also
//     asks GetRun.
type progress struct {
	fan   outbox.FanoutPublisher
	topic string
	run   uuid.UUID
	// muted is set once a publish failed: the rest of the stream is not tried.
	muted bool
}

// progressTopic is where the frames of a run go: ch:{channel} for a run that answers
// in a channel, u:{user} for one that answers only its user.
func progressTopic(channel *uuid.UUID, user uuid.UUID) string {
	if channel != nil {
		return "ch:" + channel.String()
	}
	return "u:" + user.String()
}

// running tells watchers that the run is at work and no text of its answer is on the
// way yet: at its start, and after each tool call, when what streamed before was not
// the answer.
func (p *progress) running(ctx context.Context) {
	if p.muted {
		return
	}
	p.send(ctx, frameTimeout, &realtimev1.AgentRunProgress{State: realtimev1.AgentRunState_AGENT_RUN_STATE_RUNNING})
}

// delta streams a piece of the answer's text. The first failure mutes the stream: a
// run that cannot be followed is still run.
func (p *progress) delta(ctx context.Context, text string) {
	if p.muted {
		return
	}
	p.send(ctx, frameTimeout, &realtimev1.AgentRunProgress{State: realtimev1.AgentRunState_AGENT_RUN_STATE_STREAMING, TextDelta: text})
}

// done reports the end of a run that produced answer (nil for a reply without a slip).
// The run is committed already, so it ignores the end of ctx: it is about a result, not
// about a request.
func (p *progress) done(ctx context.Context, answer *chatv1.Artifact) {
	p.muted = false
	p.send(context.WithoutCancel(ctx), doneTimeout, &realtimev1.AgentRunProgress{State: realtimev1.AgentRunState_AGENT_RUN_STATE_DONE, Answer: answer})
}

// failed reports the end of a run that will not be tried again.
func (p *progress) failed(ctx context.Context) {
	p.muted = false
	p.send(context.WithoutCancel(ctx), doneTimeout, &realtimev1.AgentRunProgress{State: realtimev1.AgentRunState_AGENT_RUN_STATE_FAILED})
}

func (p *progress) send(ctx context.Context, timeout time.Duration, frame *realtimev1.AgentRunProgress) {
	frame.RunId = p.run.String()
	raw, err := proto.Marshal(&realtimev1.ServerEvent{Event: &realtimev1.ServerEvent_AgentRunProgress{AgentRunProgress: frame}})
	if err != nil {
		slog.WarnContext(ctx, "agents: encode a progress frame", "run_id", p.run, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := p.fan.SPublish(ctx, p.topic, raw); err != nil {
		p.muted = true
		slog.WarnContext(ctx, "agents: progress frame not sent", "run_id", p.run, "state", frame.GetState().String(), "error", err)
	}
}
