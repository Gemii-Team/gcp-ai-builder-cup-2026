package app

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"kiwi/internal/agents"
	"kiwi/internal/docs"
	"kiwi/internal/knowledge"
	"kiwi/internal/meetings"
	"kiwi/internal/platform/bus"
	"kiwi/internal/tasks"
)

// jobs are the consumers each role contributes, built once from the same services as
// the handlers so that a wiring mistake (a Deps a job panics on) stops New and not the
// first message.
type jobs struct {
	// worker is doc.analyze, the meeting jobs (meeting.ingest, meeting.transcript_check,
	// meeting.transcribe), embed.index and task.draft.
	worker []bus.Job
	// agents is agent.run.
	agents []bus.Job
}

// workerJobs gathers the consumers of the worker role from the domains that own them.
func workerJobs(docDeps docs.Deps, meetingDeps meetings.Deps, knowledgeDeps knowledge.Deps, taskDeps tasks.Deps) []bus.Job {
	var all []bus.Job
	all = append(all, docs.Jobs(docDeps)...)
	all = append(all, meetings.Jobs(meetingDeps)...)
	all = append(all, knowledge.Jobs(knowledgeDeps)...)
	all = append(all, tasks.Jobs(taskDeps)...)
	return all
}

// agentJobs gathers the consumers of the agents role.
func agentJobs(d agents.Deps) []bus.Job { return agents.Jobs(d) }

// register attaches the consumers of the roles this process runs to the bus. It does
// not block, and it runs to the end before the relay starts: the relay publishes into
// the bus, and a topic that has no consumer yet acknowledges a message that nobody got.
func (a *App) register() {
	if a.has(roleWorker) {
		for _, j := range a.jobs.worker {
			a.bus.Register(j.Consumer, j.Topics, j.Handler)
		}
	}
	if a.has(roleAgents) {
		for _, j := range a.jobs.agents {
			a.bus.Register(j.Consumer, j.Topics, j.Handler)
		}
	}
}

// pollMeet asks Meet about the meetings that wait for a transcript, every interval,
// until ctx ends. A failed pass is logged and the next one is made on time; the poll
// itself reports what it could not do for each meeting.
func (a *App) pollMeet(ctx context.Context) {
	tick := time.NewTicker(a.opt.pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		a.pollMeetOnce(ctx)
	}
}

// pollMeetOnce makes one pass of the poll. A panic ends that pass only: it is logged
// with its stack, because the meeting that caused it stays pollable and a crash would
// poll it first again after every restart.
func (a *App) pollMeetOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "app: the Meet poll panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := meetings.PollMeetings(ctx, a.db, a.google, time.Now()); err != nil && ctx.Err() == nil {
		slog.Error("app: the Meet poll failed", "error", err)
	}
}
