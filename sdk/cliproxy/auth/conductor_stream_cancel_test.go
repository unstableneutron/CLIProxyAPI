package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type canceledStreamResultHook struct {
	NoopHook
	results chan Result
}

func (h *canceledStreamResultHook) OnResult(_ context.Context, result Result) {
	h.results <- result
}

func newCanceledStreamTestManager(t *testing.T) (*Manager, *Auth, *canceledStreamResultHook) {
	t.Helper()
	hook := &canceledStreamResultHook{results: make(chan Result, 4)}
	manager := NewManager(nil, nil, hook)
	auth := &Auth{ID: "canceled-stream-" + t.Name(), Provider: "canceled-stream", Status: StatusActive}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return manager, auth, hook
}

func wrapCanceledStream(manager *Manager, ctx context.Context, auth *Auth, remaining <-chan cliproxyexecutor.StreamChunk) *cliproxyexecutor.StreamResult {
	return manager.wrapStreamResult(ctx, auth, auth.Provider, "canceled-model", "canceled-model", nil, nil, remaining, OAuthModelAliasResult{}, false, cliproxyexecutor.Options{}, 1)
}

func drainCanceledStreamOutput(result *cliproxyexecutor.StreamResult) {
	for range result.Chunks {
	}
}

func assertCanceledStreamUpstreamFailure(t *testing.T, hook *canceledStreamResultHook) {
	t.Helper()
	select {
	case res := <-hook.results:
		if res.Success || res.Error == nil || res.Error.HTTPStatus != http.StatusBadGateway {
			t.Fatalf("result = %+v, want one 502 failure", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("upstream failure queued before cancellation was not recorded")
	}
	select {
	case res := <-hook.results:
		t.Fatalf("unexpected second result %+v", res)
	default:
	}
}

func TestWrapStreamResultRecordsReadyUpstreamStatusErrorAfterCancel(t *testing.T) {
	manager, auth, hook := newCanceledStreamTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	remaining := make(chan cliproxyexecutor.StreamChunk, 1)
	remaining <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream zero output"}}
	close(remaining)

	drainCanceledStreamOutput(wrapCanceledStream(manager, ctx, auth, remaining))
	assertCanceledStreamUpstreamFailure(t, hook)
}

func TestWrapStreamResultRecordsUpstreamStatusErrorQueuedAfterCancelClose(t *testing.T) {
	manager, auth, hook := newCanceledStreamTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	remaining := make(chan cliproxyexecutor.StreamChunk)

	// The wrapper closes its output without waiting for the stalled upstream.
	drainCanceledStreamOutput(wrapCanceledStream(manager, ctx, auth, remaining))
	remaining <- cliproxyexecutor.StreamChunk{Err: &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream zero output"}}
	close(remaining)
	assertCanceledStreamUpstreamFailure(t, hook)
}

func TestWrapStreamResultIgnoresCancellationErrorQueuedAfterCancelClose(t *testing.T) {
	manager, auth, hook := newCanceledStreamTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	remaining := make(chan cliproxyexecutor.StreamChunk)

	drainCanceledStreamOutput(wrapCanceledStream(manager, ctx, auth, remaining))
	remaining <- cliproxyexecutor.StreamChunk{Err: context.Canceled}
	// The unbuffered send completes only after the drain finished the previous chunk.
	remaining <- cliproxyexecutor.StreamChunk{Payload: []byte("ignored")}
	close(remaining)
	select {
	case res := <-hook.results:
		t.Fatalf("cancellation artifact recorded result %+v", res)
	default:
	}
}
