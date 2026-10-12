// Command llmreplay is the recorded LLM of the smoke stack: an OpenAI-compatible server
// that answers K.I.W.I.'s model calls from fixture files instead of a model, so that a
// run needs no paid model session, and fails loudly when a request has no fixture, so
// that the fixtures cannot drift from what the product asks.
//
// # Endpoints
//
// The ones the LLM adapter calls (the base URLs carry no /v1):
//
//	POST /v1/chat/completions    JSON, or SSE with "stream": true, from the same fixture
//	POST /v1/embeddings          768-dimension vectors of hashed character trigrams
//	POST /v1/audio/transcriptions  multipart; the transcript is keyed by the audio's SHA-256
//	GET  /healthz                200 once the fixtures have loaded
//
// A request no fixture answers is HTTP 599 (a status no model sends) with a JSON body
// {"error": {"message", "type": "fixture_unmatched", "code": 599, "schema", "stage",
// "looked_for", "last_user_message"}}: the schema name, the stage, what it looked for
// and the head (at most 120 runes, whitespace collapsed) of the last user message. The
// message, which names the schema and the stage, comes first because llm.HTTPError
// repeats only 200 bytes of a body; and it never says "llmreplay", because the adapter
// replaces the host name of its endpoint by <host> in what it reports. The log has the
// same summary. Neither ever holds a header, an Authorization value or any other part
// of a request. Embeddings never need a fixture and are never a 599.
//
// # Running
//
//	llmreplay [-addr :8089] [-fixtures /fixtures]
//
// -addr and -fixtures default to $LLMREPLAY_ADDR and $LLMREPLAY_FIXTURES, then to
// :8089 and /fixtures. A fixture directory that cannot be read or holds a file that
// does not parse (or breaks a rule below) makes the server exit 1 naming the file;
// nothing is served from a half-loaded set. SIGTERM and SIGINT shut it down gracefully.
//
// # Fixtures
//
// The directory holds *.json files, read in name order (10-files.json before
// 20-meetings.json); other files are ignored. A file is one fixture object or a list
// of them. The first fixture that fits a request answers it, so put the specific
// before the general. There are two kinds.
//
// A chat fixture answers /v1/chat/completions:
//
//	{
//	  "match": {
//	    "schema":   "file_summary",              // response_format.json_schema.name
//	    "stage":    "agent_tool",                // or "agent_final", see below
//	    "contains": "warehouse"                  // a string or a list; all must occur
//	  },
//	  "response": { ...a chat.completion body... }
//	}
//
// Every field of "match" is optional and all that are set must hold; no "match" fits
// every request. "contains" is searched in the text of all the request's messages
// (system, user, assistant and tool, joined by newlines), case-sensitively. The names
// are file_summary, meeting_summary, kb_answer and channel_summary; the agents send no
// schema and are told apart by stage: "agent_tool" is a request that declares tools
// and has no tool message yet (the model is to call a tool), "agent_final" is one with
// a tool message in it (the model is to answer). A request with neither has no stage.
//
// "response" is the body of a chat completion, as recorded from a real model or cut
// down to what the product reads: choices[0].message.content (or tool_calls) and,
// optionally, usage. The server fills in what a server always sends and the fixture
// leaves out: id, object, model (the request's), choices[0].index, message.role,
// finish_reason ("stop", or "tool_calls" when there are tool calls), and for each tool
// call its id and "type": "function". The body must have exactly one choice.
// Anything else in it (reasoning_content, provider fields, usage) is passed through,
// and a stream carries the reasoning, the usage and the finish reason as a real one.
//
// Where the wire carries JSON inside a string, a fixture may write the JSON itself:
// message.content for a structured answer, and tool_calls[].function.arguments:
//
//	"message": {"content": {"summary": "...", "key_points": ["..."]}}
//	"message": {"tool_calls": [{"function": {"name": "search_knowledge",
//	                                         "arguments": {"question": "..."}}}]}
//
// An id that differs per database is never in a fixture: a UUID anywhere in one
// makes the directory fail to load. The ids a response must echo are taken from the
// request, with placeholders in any string of the response (not in "match"):
//
//	{{chunk:TEXT}}   the id of the first <chunk id="…"> block of the request whose text
//	                 contains TEXT: the citations of a kb_answer
//	{{uuid:PREFIX}}  the UUID that follows PREFIX in the request: for a tool argument,
//	                 "the channel with id " or "Nok = " in the agent's input
//
// A kb_answer cites by passage, and its [n] markers are plain text in answer_markdown
// (n is the position in the fixture's own citations list):
//
//	"content": {
//	  "answer_markdown": "Staff get 15 days of annual leave [1].",
//	  "citations": [{"chunk_id": "{{chunk:15 days of annual leave}}",
//	                 "quote": "15 days of annual leave"}]
//	}
//
// A placeholder that matches nothing in the request makes it unmatched (HTTP 599, with
// the fixture and the text it looked for in "looked_for"), so a retrieval that changed
// is a loud failure and never a citation of the wrong chunk.
//
// A transcription fixture answers /v1/audio/transcriptions, keyed by the lower- or
// upper-case SHA-256 of the file bytes of the upload (the adapter sends 20 second
// windows re-wrapped in a canonical 44-byte header, which is the same bytes as the
// source file for a canonical file shorter than 20 s; an unmatched upload's 599 body
// carries the hash it saw). An empty text is an empty transcript:
//
//	{"audio_sha256": "<64 hex digits>", "text": "Good morning everyone."}
//
// The unmatched 599 for audio names the first 8 hex digits of the hash.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	defaultAddr     = ":8089"
	defaultFixtures = "/fixtures"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stderr)
	stop()
	os.Exit(code)
}

// run loads the fixtures and serves them until ctx is done. It returns the exit code:
// 0 after a clean shutdown, 1 when the fixtures or the listener fail, 2 for bad flags.
func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) int {
	fs := flag.NewFlagSet("llmreplay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", cmp.Or(getenv("LLMREPLAY_ADDR"), defaultAddr), "address to listen on (env LLMREPLAY_ADDR)")
	dir := fs.String("fixtures", cmp.Or(getenv("LLMREPLAY_FIXTURES"), defaultFixtures), "directory of fixture files (env LLMREPLAY_FIXTURES)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	fx, err := loadFixtures(*dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "llmreplay: %v\n", err)
		return 1
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "llmreplay: %v\n", err)
		return 1
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	log.Info("serving", "addr", ln.Addr().String(), "fixtures", *dir, "chat", len(fx.chat), "transcriptions", len(fx.audio))
	return serve(ctx, ln, newHandler(fx, log), stderr)
}

// serve answers on ln until ctx is done, then lets requests in flight finish (for at
// most 5 s) and returns the exit code.
func serve(ctx context.Context, ln net.Listener, h http.Handler, stderr io.Writer) int {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	failed := make(chan error, 1)
	go func() { failed <- srv.Serve(ln) }()
	select {
	case err := <-failed:
		_, _ = fmt.Fprintf(stderr, "llmreplay: %v\n", err)
		return 1
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_, _ = fmt.Fprintf(stderr, "llmreplay: shutdown: %v\n", err)
		return 1
	}
	return 0
}
