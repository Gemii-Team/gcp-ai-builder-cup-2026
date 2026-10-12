package bus_test

import (
	"context"
	"testing"

	"kiwi/internal/platform/bus"
)

func TestKeepAlive_IsNoOpWithoutAdapterAndCallsTheInjectedFunction(t *testing.T) {
	bus.KeepAlive(context.Background()) // must not panic

	var extended int
	ctx := bus.WithKeepAlive(context.Background(), func() { extended++ })
	bus.KeepAlive(ctx)
	bus.KeepAlive(ctx)
	if extended != 2 {
		t.Fatalf("injected keep-alive calls = %d, want 2", extended)
	}
}
