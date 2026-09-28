package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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
			if !ok || err.ErrorCode() != "abort" || err.Error() != "stop" || err.StatusCode() != 429 || !err.Retryable() {
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

func TestCallPluginPreservesStatusFromNewErrorEnvelope(t *testing.T) {
	raw, errMarshal := pluginabi.NewErrorEnvelope("insufficient_quota", "plan limit reached", http.StatusForbidden)
	if errMarshal != nil {
		t.Fatalf("NewErrorEnvelope() error = %v", errMarshal)
	}
	_, errCall := callPlugin[rpcEmptyResponse](context.Background(), staticEnvelopePluginClient{raw: raw}, pluginabi.MethodExecutorExecute, rpcEmptyResponse{})
	if errCall == nil {
		t.Fatal("callPlugin returned nil error")
	}
	if got := errCall.Error(); got != "plan limit reached" {
		t.Fatalf("error = %q, want plan limit reached", got)
	}
	statusProvider, ok := errCall.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode", errCall)
	}
	if got := statusProvider.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", got, http.StatusForbidden)
	}
}

func TestMarshalRPCErrorPreservesHTTPStatus(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		raw := marshalRPCError("host_call_failed", "synthetic", status)
		var env pluginabi.Envelope
		if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
			t.Fatalf("unmarshal envelope: %v", errUnmarshal)
		}
		if env.OK {
			t.Fatal("expected envelope OK=false")
		}
		if env.Error == nil {
			t.Fatal("expected non-nil Error in envelope")
		}
		if env.Error.HTTPStatus != status {
			t.Fatalf("HTTPStatus = %d, want %d", env.Error.HTTPStatus, status)
		}
		_, errDecode := decodeEnvelopeResult[rpcEmptyResponse](env)
		if errDecode == nil {
			t.Fatal("expected decode error")
		}
		statusProvider, ok := errDecode.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("decoded error does not expose StatusCode: %T", errDecode)
		}
		if got := statusProvider.StatusCode(); got != status {
			t.Fatalf("StatusCode = %d, want %d", got, status)
		}
	}
}

func TestCallPluginSchedulerPickCompatibility(t *testing.T) {
	tests := []struct {
		name         string
		resultJSON   string
		wantAuthID   string
		wantDelegate string
		wantHandled  bool
		wantReject   bool
		wantCode     string
		wantReason   string
	}{
		{
			name:         "legacy pascal case auth id",
			resultJSON:   `{"AuthID":"auth-1","DelegateBuiltin":"","Handled":true}`,
			wantAuthID:   "auth-1",
			wantDelegate: "",
			wantHandled:  true,
			wantReject:   false,
		},
		{
			name:         "legacy pascal case delegate",
			resultJSON:   `{"AuthID":"","DelegateBuiltin":"round-robin","Handled":true}`,
			wantAuthID:   "",
			wantDelegate: "round-robin",
			wantHandled:  true,
			wantReject:   false,
		},
		{
			name:         "snake case auth id",
			resultJSON:   `{"auth_id":"auth-2","delegate_builtin":"","handled":true}`,
			wantAuthID:   "auth-2",
			wantDelegate: "",
			wantHandled:  true,
			wantReject:   false,
		},
		{
			name:         "snake case delegate",
			resultJSON:   `{"auth_id":"","delegate_builtin":"fill-first","handled":true}`,
			wantAuthID:   "",
			wantDelegate: "fill-first",
			wantHandled:  true,
			wantReject:   false,
		},
		{
			name:        "snake case terminal rejection",
			resultJSON:  `{"handled":true,"reject":true,"reject_code":"quota_exceeded","reject_reason":"quota exhausted"}`,
			wantHandled: true,
			wantReject:  true,
			wantCode:    "quota_exceeded",
			wantReason:  "quota exhausted",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := pluginabi.Envelope{
				OK:     true,
				Result: json.RawMessage(tc.resultJSON),
			}
			raw, errMarshal := json.Marshal(env)
			if errMarshal != nil {
				t.Fatalf("marshal envelope: %v", errMarshal)
			}

			client := staticEnvelopePluginClient{raw: raw}
			resp, errCall := callPlugin[pluginapi.SchedulerPickResponse](context.Background(), client, pluginabi.MethodSchedulerPick, pluginapi.SchedulerPickRequest{})
			if errCall != nil {
				t.Fatalf("callPlugin() error = %v", errCall)
			}
			if resp.AuthID != tc.wantAuthID {
				t.Fatalf("AuthID = %q, want %q", resp.AuthID, tc.wantAuthID)
			}
			if resp.DelegateBuiltin != tc.wantDelegate {
				t.Fatalf("DelegateBuiltin = %q, want %q", resp.DelegateBuiltin, tc.wantDelegate)
			}
			if resp.Handled != tc.wantHandled {
				t.Fatalf("Handled = %v, want %v", resp.Handled, tc.wantHandled)
			}
			if resp.Reject != tc.wantReject {
				t.Fatalf("Reject = %v, want %v", resp.Reject, tc.wantReject)
			}
			if resp.RejectCode != tc.wantCode {
				t.Fatalf("RejectCode = %q, want %q", resp.RejectCode, tc.wantCode)
			}
			if resp.RejectReason != tc.wantReason {
				t.Fatalf("RejectReason = %q, want %q", resp.RejectReason, tc.wantReason)
			}
		})
	}
}
