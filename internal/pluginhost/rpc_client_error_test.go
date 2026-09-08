package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

type staticEnvelopePluginClient struct {
	raw []byte
}

func (c staticEnvelopePluginClient) Call(context.Context, string, []byte) ([]byte, error) {
	return c.raw, nil
}

func (c staticEnvelopePluginClient) Shutdown() {}

func TestDecodeEnvelopeResultPreservesPluginHTTPStatus(t *testing.T) {
	_, errDecode := decodeEnvelopeResult[rpcEmptyResponse](pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       "plugin_error",
			Message:    "license required",
			HTTPStatus: http.StatusForbidden,
		},
	})
	if errDecode == nil {
		t.Fatal("decodeEnvelopeResult returned nil error")
	}
	if got := errDecode.Error(); got != "license required" {
		t.Fatalf("error = %q, want license required", got)
	}
	statusProvider, ok := errDecode.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", errDecode)
	}
	if got := statusProvider.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
}

func TestCallPluginReturnsPluginErrorWithoutMethodWrapper(t *testing.T) {
	raw, errMarshal := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       "plugin_error",
			Message:    "license required",
			HTTPStatus: http.StatusForbidden,
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal envelope: %v", errMarshal)
	}
	_, errCall := callPlugin[rpcEmptyResponse](context.Background(), staticEnvelopePluginClient{raw: raw}, pluginabi.MethodExecutorExecuteStream, rpcEmptyResponse{})
	if errCall == nil {
		t.Fatal("callPlugin returned nil error")
	}
	if got := errCall.Error(); got != "license required" {
		t.Fatalf("error = %q, want license required", got)
	}
	statusProvider, ok := errCall.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", errCall)
	}
	if got := statusProvider.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
}

func TestIsPluginErrorEnvelopeAcceptsNonzeroReturnEnvelope(t *testing.T) {
	raw := marshalRPCError("plugin_error", "upstream failed")
	if !isPluginErrorEnvelope(raw) {
		t.Fatalf("isPluginErrorEnvelope(%s) = false, want true", raw)
	}
	if isPluginErrorEnvelope([]byte(`not json`)) {
		t.Fatal("isPluginErrorEnvelope accepted invalid JSON")
	}
}

func TestPluginFailureScopeAndRetryHints(t *testing.T) {
	for _, scope := range []string{"", "request", "credential", "model", "future-scope"} {
		t.Run(scope, func(t *testing.T) {
			ms := int64(1234)
			_, errDecode := decodeEnvelopeResult[rpcEmptyResponse](pluginabi.Envelope{Error: &pluginabi.Error{
				Code: "abort", Message: "stop", HTTPStatus: 429, Scope: scope,
				Retryable: true, RetryAfterMS: &ms,
			}})
			err, ok := errDecode.(rpcError)
			if !ok || err.Code != "abort" || err.Error() != "stop" || err.StatusCode() != 429 || !err.Retryable() {
				t.Fatalf("failure fields lost: %#v", errDecode)
			}
			if err.IsRequestScoped() != (scope == "request") || err.IsCredentialScoped() != (scope == "credential") {
				t.Fatalf("incorrect scope: %#v", err)
			}
			if got := err.RetryAfter(); got == nil || *got != 1234*time.Millisecond {
				t.Fatalf("retry hint = %v", got)
			}
			*err.RetryAfter() = 0
			if *err.RetryAfter() != 1234*time.Millisecond {
				t.Fatal("caller mutated retained retry hint")
			}
		})
	}
	for _, ms := range []int64{-1, 0, 1<<63 - 1} {
		err := decodePluginFailure(&pluginabi.Error{RetryAfterMS: &ms}, "").(rpcError)
		if (err.RetryAfter() != nil) != (ms == 0) {
			t.Fatalf("invalid or explicit-zero retry hint mishandled: %d", ms)
		}
	}
}

func TestHostStreamCallbacksPreserveFailure(t *testing.T) {
	for _, method := range []string{pluginabi.MethodHostStreamEmit, pluginabi.MethodHostStreamClose} {
		t.Run(method, func(t *testing.T) {
			host := New()
			id, chunks, cleanup := host.streams.open(context.Background())
			defer cleanup()
			raw, errMarshal := json.Marshal(map[string]any{
				"stream_id": id, "error": "legacy text",
				"failure": pluginabi.Error{Message: "aborted", Scope: "request", HTTPStatus: 400},
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			var errCall error
			if method == pluginabi.MethodHostStreamEmit {
				_, errCall = host.callHostStreamEmit(context.Background(), raw)
			} else {
				_, errCall = host.callHostStreamClose(raw)
			}
			if errCall != nil {
				t.Fatal(errCall)
			}
			chunk := <-chunks
			err, ok := chunk.Err.(rpcError)
			if !ok || !err.IsRequestScoped() || err.StatusCode() != 400 || err.Error() != "aborted" {
				t.Fatalf("stream lost typed error: %#v", chunk.Err)
			}
		})
	}
}
