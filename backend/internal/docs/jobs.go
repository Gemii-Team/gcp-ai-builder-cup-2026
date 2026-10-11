package docs

import (
	"kiwi/internal/platform/bus"
	"kiwi/internal/platform/events"
	"kiwi/internal/platform/outbox"
)

// consumerAnalyze is the job that summarises an uploaded file.
const consumerAnalyze = "doc.analyze"

// finalAttempt is the delivery the bus dead-letters a message after: an analysis that
// still fails on it is recorded as failed, because nothing will deliver the message
// again and the file would show a spinner for ever.
const finalAttempt = 5

// Jobs returns the consumers of the docs domain for the composition root to register:
// doc.analyze, which reads a file when doc.uploaded announces it and posts its summary.
func Jobs(d Deps) []bus.Job {
	a := &analyzer{db: d.DB, blob: d.Blob, chat: d.Chat, llm: d.LLM}
	return []bus.Job{{
		Consumer: consumerAnalyze,
		Topics:   []string{events.TopicDocUploaded},
		Handler:  outbox.Once(d.DB, consumerAnalyze, a.prepare),
	}}
}
