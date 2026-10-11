package bus_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"kiwi/internal/platform/bus"
)

var errBoom = errors.New("boom")

// instant returns options that retry at once and fail the test on any dead letter;
// tests that expect one replace OnDead.
func instant(t *testing.T) bus.MemoryOptions {
	t.Helper()
	return bus.MemoryOptions{
		MaxAttempts: 5,
		Backoff:     []time.Duration{0},
		OnDead: func(consumer string, m bus.Message, err error) {
			t.Errorf("unexpected dead letter: consumer %q, message %q: %v", consumer, m.ID, err)
		},
	}
}

func TestMemory_NackRedeliversWithIncrementedAttempt(t *testing.T) {
	m := bus.NewMemory(instant(t))
	var attempts []int
	m.Register("c", []string{"t"}, func(_ context.Context, msg bus.Message) error {
		attempts = append(attempts, msg.Attempt)
		if len(attempts) == 1 {
			return errBoom
		}
		return nil
	})

	if err := m.Publish(context.Background(), bus.Message{ID: "1", Topic: "t"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if want := []int{1, 2}; !slices.Equal(attempts, want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
}

func TestMemory_AckedMessageNotRedelivered(t *testing.T) {
	m := bus.NewMemory(instant(t))
	var calls int
	m.Register("c", []string{"t"}, func(_ context.Context, msg bus.Message) error {
		calls++
		return nil
	})

	if err := m.Publish(context.Background(), bus.Message{ID: "1", Topic: "t"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
}

func TestMemory_PublishBlocksUntilEveryConsumerSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := bus.NewMemory(instant(t))
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()

		var fastCalls atomic.Int32
		var slowSettled atomic.Bool
		m.Register("fast", []string{"t"}, func(context.Context, bus.Message) error {
			fastCalls.Add(1)
			return nil
		})
		m.Register("slow", []string{"t"}, func(context.Context, bus.Message) error {
			<-release
			slowSettled.Store(true)
			return nil
		})

		done := make(chan error, 1)
		go func() { done <- m.Publish(context.Background(), bus.Message{ID: "1", Topic: "t"}) }()

		// Every goroutine in the bubble is now blocked: the fast consumer has acked and
		// the slow one waits on release.
		synctest.Wait()
		if got := fastCalls.Load(); got != 1 {
			t.Fatalf("fast consumer calls = %d, want 1", got)
		}
		select {
		case err := <-done:
			t.Fatalf("Publish returned (%v) while the slow consumer was still running", err)
		default:
		}

		unblock()
		if err := <-done; err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if !slowSettled.Load() {
			t.Fatal("Publish returned before the slow consumer settled")
		}
	})
}

func TestMemory_DeadLettersAfterMaxAttempts(t *testing.T) {
	var deadCalls int
	var deadConsumer string
	var deadMsg bus.Message
	var deadErr error
	opt := instant(t)
	opt.OnDead = func(consumer string, m bus.Message, err error) {
		deadCalls++
		deadConsumer, deadMsg, deadErr = consumer, m, err
	}
	m := bus.NewMemory(opt)
	var attempts []int
	m.Register("always-fails", []string{"t"}, func(_ context.Context, msg bus.Message) error {
		attempts = append(attempts, msg.Attempt)
		return errBoom
	})

	if err := m.Publish(context.Background(), bus.Message{ID: "7", Topic: "t"}); err != nil {
		t.Fatalf("Publish must settle a dead-lettered consumer without an error: %v", err)
	}
	if want := []int{1, 2, 3, 4, 5}; !slices.Equal(attempts, want) {
		t.Fatalf("attempts = %v, want %v", attempts, want)
	}
	if deadCalls != 1 {
		t.Fatalf("OnDead calls = %d, want 1", deadCalls)
	}
	if deadConsumer != "always-fails" || deadMsg.ID != "7" || deadMsg.Attempt != 5 || !errors.Is(deadErr, errBoom) {
		t.Fatalf("OnDead(%q, id=%q attempt=%d, %v), want (always-fails, 7, 5, boom)", deadConsumer, deadMsg.ID, deadMsg.Attempt, deadErr)
	}
}

func TestMemory_DefaultsRetryOnTheDocumentedSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var deadCalls int
		m := bus.NewMemory(bus.MemoryOptions{
			OnDead: func(string, bus.Message, error) { deadCalls++ },
		})
		start := time.Now()
		var offsets []time.Duration
		m.Register("c", []string{"t"}, func(context.Context, bus.Message) error {
			offsets = append(offsets, time.Since(start))
			return errBoom
		})

		if err := m.Publish(context.Background(), bus.Message{ID: "1", Topic: "t"}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		want := []time.Duration{0, time.Second, 6 * time.Second, 36 * time.Second, 156 * time.Second}
		if !slices.Equal(offsets, want) {
			t.Fatalf("attempt offsets = %v, want %v (1s, 5s, 30s, 2m between attempts)", offsets, want)
		}
		if deadCalls != 1 {
			t.Fatalf("OnDead calls = %d, want 1", deadCalls)
		}
	})
}

func TestMemory_ShortBackoffListRepeatsItsLastEntry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opt := instant(t)
		opt.MaxAttempts = 4
		opt.Backoff = []time.Duration{time.Second, 2 * time.Second}
		var deadCalls int
		opt.OnDead = func(string, bus.Message, error) { deadCalls++ }
		m := bus.NewMemory(opt)
		start := time.Now()
		var offsets []time.Duration
		m.Register("c", []string{"t"}, func(context.Context, bus.Message) error {
			offsets = append(offsets, time.Since(start))
			return errBoom
		})

		if err := m.Publish(context.Background(), bus.Message{ID: "1", Topic: "t"}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		want := []time.Duration{0, time.Second, 3 * time.Second, 5 * time.Second}
		if !slices.Equal(offsets, want) {
			t.Fatalf("attempt offsets = %v, want %v", offsets, want)
		}
		if deadCalls != 1 {
			t.Fatalf("OnDead calls = %d, want 1", deadCalls)
		}
	})
}

func TestMemory_NoConsumers_PublishReturnsNil(t *testing.T) {
	m := bus.NewMemory(instant(t))
	var otherTopicCalls int
	m.Register("other", []string{"other.topic"}, func(context.Context, bus.Message) error {
		otherTopicCalls++
		return nil
	})

	if err := m.Publish(context.Background(), bus.Message{ID: "1", Topic: "nobody.listens"}); err != nil {
		t.Fatalf("Publish on a topic without consumers: %v", err)
	}
	if otherTopicCalls != 0 {
		t.Fatalf("a consumer of another topic was called %d times", otherTopicCalls)
	}
}

func TestMemory_CancelledContext_ReturnsErrorWithoutDeadLetter(t *testing.T) {
	t.Run("before the first delivery", func(t *testing.T) {
		m := bus.NewMemory(instant(t))
		var calls int
		m.Register("c", []string{"t"}, func(context.Context, bus.Message) error {
			calls++
			return nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := m.Publish(ctx, bus.Message{ID: "1", Topic: "t"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Publish error = %v, want context.Canceled", err)
		}
		if calls != 0 {
			t.Fatalf("handler calls = %d, want 0", calls)
		}
	})

	t.Run("when the handler fails and the context ended meanwhile", func(t *testing.T) {
		m := bus.NewMemory(instant(t))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls int
		m.Register("c", []string{"t"}, func(context.Context, bus.Message) error {
			calls++
			cancel()
			return errBoom
		})

		err := m.Publish(ctx, bus.Message{ID: "1", Topic: "t"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Publish error = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Fatalf("handler calls = %d, want 1 (no retry after cancellation)", calls)
		}
	})

	// On shutdown a handler fails because its context was cancelled. If that was its
	// last attempt the message must not be dead-lettered and Publish must not return
	// nil: the outbox relay would mark the row published and the message would be lost.
	t.Run("when the handler fails its last attempt and the context ended meanwhile", func(t *testing.T) {
		opt := instant(t) // OnDead fails the test
		opt.MaxAttempts = 1
		m := bus.NewMemory(opt)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls int
		m.Register("c", []string{"t"}, func(context.Context, bus.Message) error {
			calls++
			cancel()
			return errBoom
		})

		err := m.Publish(ctx, bus.Message{ID: "1", Topic: "t"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Publish error = %v, want context.Canceled", err)
		}
		if calls != 1 {
			t.Fatalf("handler calls = %d, want 1", calls)
		}
	})

	t.Run("while waiting for the next retry", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			opt := instant(t)
			opt.Backoff = []time.Duration{time.Hour}
			m := bus.NewMemory(opt)
			var calls int
			m.Register("c", []string{"t"}, func(context.Context, bus.Message) error {
				calls++
				return errBoom
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan error, 1)
			go func() { done <- m.Publish(ctx, bus.Message{ID: "1", Topic: "t"}) }()
			synctest.Wait() // the consumer failed once and Publish sleeps until the retry
			cancel()

			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("Publish error = %v, want context.Canceled", err)
			}
			if calls != 1 {
				t.Fatalf("handler calls = %d, want 1", calls)
			}
		})
	})
}

func TestMemory_Subscribe_RegistersUntilContextIsDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := bus.NewMemory(instant(t))
		var calls atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		subscribed := make(chan error, 1)
		go func() {
			subscribed <- m.Subscribe(ctx, "c", []string{"t"}, func(context.Context, bus.Message) error {
				calls.Add(1)
				return nil
			})
		}()
		synctest.Wait() // Subscribe has registered and blocks on ctx

		if err := m.Publish(context.Background(), bus.Message{ID: "1", Topic: "t"}); err != nil {
			t.Fatalf("Publish while subscribed: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("calls while subscribed = %d, want 1", got)
		}

		cancel()
		if err := <-subscribed; err != nil {
			t.Fatalf("Subscribe after its context ended = %v, want nil", err)
		}
		if err := m.Publish(context.Background(), bus.Message{ID: "2", Topic: "t"}); err != nil {
			t.Fatalf("Publish after unsubscribe: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("calls after unsubscribe = %d, want still 1", got)
		}
	})
}
