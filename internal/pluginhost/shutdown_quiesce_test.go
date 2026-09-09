package pluginhost

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type quiesceShutdownClient struct {
	started chan struct{}
	release chan struct{}
	done    chan struct{}
	retired atomic.Bool
}

func (c *quiesceShutdownClient) Call(_ context.Context, method string, _ []byte) ([]byte, error) {
	if method == pluginabi.MethodPluginQuiesce {
		close(c.started)
		<-c.release
	}
	return []byte(`{}`), nil
}

func (c *quiesceShutdownClient) retire()   { c.retired.Store(true) }
func (c *quiesceShutdownClient) Shutdown() { close(c.done) }

func TestShutdownQuiescesBeforeRetiringCallbacksAfterCancellation(t *testing.T) {
	inner := &quiesceShutdownClient{started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{})}
	guard := newGuardedPluginClient(inner)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	shutdownPluginClient(ctx, guard)
	waitForHostTestSignal(t, inner.started, "quiesce started")
	if inner.retired.Load() {
		t.Fatal("callbacks retired while quiesce still needs them to cancel an upstream read")
	}
	select {
	case <-inner.done:
		t.Fatal("plugin shutdown ran before quiesce drained")
	default:
	}
	close(inner.release)
	waitForHostTestSignal(t, inner.done, "shutdown after quiesce")
	if !inner.retired.Load() {
		t.Fatal("callbacks were not retired after quiesce completed")
	}
}
