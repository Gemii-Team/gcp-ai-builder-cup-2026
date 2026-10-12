package knowledge

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"kiwi/internal/chat"
	"kiwi/internal/docs"
	"kiwi/internal/meetings"
	"kiwi/internal/platform/authz"
	"kiwi/internal/platform/db"
	"kiwi/internal/platform/errs"
	"kiwi/internal/platform/llm"
)

const (
	// embedBatch is how many chunks go to the model in one call. The adapter sends a
	// call as one request and a CPU embeds about 125 tokens a second, so eight chunks
	// of 512 tokens take half a minute: long enough to be worth batching, short enough
	// that a call fits its deadline and a stopped run has lost little.
	embedBatch = 8
	// embedTimeout bounds one embedding call; the adapter has no timeout of its own.
	embedTimeout = 3 * time.Minute
	// embedDim is the width of the vector column.
	embedDim = 768
)

// Deps are what New and Jobs build the knowledge domain from. A Service that only
// reindexes (`kiwi reindex`) needs DB and LLM; indexing needs them all.
type Deps struct {
	DB *db.DB
	// LLM embeds chunks and names the model that did.
	LLM llm.Client
	// Authz decides who may index a source.
	Authz *authz.Authz
	// Chat, Meetings and Docs are where the indexer reads the sources it cuts into
	// chunks.
	Chat     chat.Service
	Meetings meetings.Service
	Docs     docs.Service
}

type service struct {
	db       *db.DB
	llm      llm.Client
	authz    *authz.Authz
	chat     chat.Service
	meetings meetings.Service
	docs     docs.Service
	locks    *keyedLock
}

// New returns the knowledge domain.
func New(d Deps) Service {
	return newService(d)
}

func newService(d Deps) *service {
	return &service{db: d.DB, llm: d.LLM, authz: d.Authz, chat: d.Chat, meetings: d.Meetings, docs: d.Docs, locks: newKeyedLock()}
}

var _ Service = (*service)(nil)

// complete reports a Service built without what indexing reads from.
func (s *service) complete() error {
	if s.db == nil || s.llm == nil || s.authz == nil || s.chat == nil || s.meetings == nil || s.docs == nil {
		return fmt.Errorf("knowledge: indexing needs every dependency; this Service was built to reindex: %w", errs.ErrInvalid)
	}
	return nil
}

// keyedLock serialises the runs that write one source. Two runs of one source must
// not overlap: each reads the source, then writes what it read and deletes what it
// did not produce, so the later writer of two that read different moments would undo
// the newer. The second run waits and then reads the source as the first left it.
// It holds per Service instance, not per process: Jobs builds the one Service that runs
// the embed.index job, which is the only caller of Index, so every run shares it. A
// second Service built from the same Deps has a lock of its own.
type keyedLock struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	held chan struct{} // holds one token while the key is locked
	refs int
}

func newKeyedLock() *keyedLock { return &keyedLock{entries: map[string]*lockEntry{}} }

// lock waits for key and returns the function that releases it, or ctx's error when ctx
// ends first.
func (k *keyedLock) lock(ctx context.Context, key string) (unlock func(), err error) {
	k.mu.Lock()
	e := k.entries[key]
	if e == nil {
		e = &lockEntry{held: make(chan struct{}, 1)}
		k.entries[key] = e
	}
	e.refs++
	k.mu.Unlock()

	release := func() {
		k.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(k.entries, key)
		}
		k.mu.Unlock()
	}
	select {
	case e.held <- struct{}{}:
		return func() {
			<-e.held
			release()
		}, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

// lockKey names a source for keyedLock; a chat window is a source of its own.
func lockKey(kind string, id uuid.UUID, window *window) string {
	if window != nil {
		return kind + "/" + id.String() + "/" + window.start.Format(time.RFC3339)
	}
	return kind + "/" + id.String()
}
