package auth

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type codedPluginDenial struct {
	code    string
	status  int
	request bool
}

func (e codedPluginDenial) Error() string {
	if e.status == 403 {
		return `{"error":{"type":"permission_error","message":"denied"}}`
	}
	return `{"error":{"type":"invalid_request_error","message":"denied"}}`
}
func (e codedPluginDenial) ErrorCode() string     { return e.code }
func (e codedPluginDenial) StatusCode() int       { return e.status }
func (e codedPluginDenial) IsRequestScoped() bool { return e.request }

func TestPluginDenialCodeSurvivesWrappers(t *testing.T) {
	for _, tc := range []struct {
		code           string
		status         int
		request, model bool
	}{
		{"unsupported_model", 400, false, true},
		{"unsupported_model", 422, false, true},
		{"unsupported_model", 500, false, false},
		{"invalid_request_error", 400, false, false},
		{"upstream_http", 400, false, false},
		{"upgrade_required", 403, false, false},
		{"unsupported_model", 400, true, true},
	} {
		t.Run(fmt.Sprintf("%s/%d/%t", tc.code, tc.status, tc.request), func(t *testing.T) {
			err := fmt.Errorf("bootstrap: %w", codedPluginDenial{tc.code, tc.status, tc.request})
			if isModelSupportError(err) != tc.model {
				t.Fatal("incorrect model support classification")
			}
			result := resultErrorFromError(err)
			wantCode := tc.code
			if tc.request || (tc.status == 400 && !tc.model) {
				wantCode = requestScopedErrorCode
			}
			if result.Code != wantCode || result.HTTPStatus != tc.status {
				t.Fatalf("result = %+v, want code %s/status %d", result, wantCode, tc.status)
			}
			if tc.model && !tc.request && (isRequestInvalidError(err) || !isModelSupportResultError(result)) {
				t.Fatal("structured model denial was demoted to a request fault")
			}
		})
	}
	if !isModelSupportError(fmt.Errorf("wrapped: %w", &Error{Code: "unsupported_model", HTTPStatus: 400, Message: "denied"})) {
		t.Fatal("core error code lost through wrapper")
	}
}

func TestPluginDenialCooldownIsolationAndRecovery(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, tc := range []struct {
		code   string
		status int
		delay  time.Duration
	}{
		{"unsupported_model", 400, 12 * time.Hour},
		{"upgrade_required", 403, 30 * time.Minute},
	} {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disabled=%t", tc.code, disabled), func(t *testing.T) {
				m := NewManager(nil, nil, nil)
				ctx := context.Background()
				for _, id := range []string{"denied-account", "other-account", "rotated-account"} {
					if _, err := m.Register(ctx, &Auth{ID: id, Provider: "commandcode", Metadata: map[string]any{"disable_cooling": disabled}}); err != nil {
						t.Fatal(err)
					}
				}
				m.MarkResult(ctx, Result{AuthID: "denied-account", Provider: "commandcode", Model: "model-x", CredentialScope: tc.status == 403,
					Error: resultErrorFromError(codedPluginDenial{tc.code, tc.status, false})})
				a, _ := m.GetByID("denied-account")
				state := a.ModelStates["model-x"]
				if state == nil {
					t.Fatal("missing model state")
				}
				if disabled {
					if !state.NextRetryAfter.IsZero() {
						t.Fatal("disabled cooling acquired deadline")
					}
					if blocked, _, _ := isAuthBlockedForModel(a, "model-x", state.UpdatedAt); blocked {
						t.Fatal("disabled cooling blocked credential")
					}
					return
				}
				if !state.NextRetryAfter.Equal(state.UpdatedAt.Add(tc.delay)) {
					t.Fatalf("cooldown duration = %v, want %v", state.NextRetryAfter.Sub(state.UpdatedAt), tc.delay)
				}
				if blocked, _, _ := isAuthBlockedForModel(a, "model-x", state.NextRetryAfter.Add(-time.Nanosecond)); !blocked {
					t.Fatal("denied pair eligible before expiry")
				}
				if blocked, _, _ := isAuthBlockedForModel(a, "model-x", state.NextRetryAfter); blocked {
					t.Fatal("denied pair still blocked at expiry")
				}
				if blocked, _, _ := isAuthBlockedForModel(a, "model-y", state.UpdatedAt); blocked {
					t.Fatal("unrelated model blocked")
				}
				for _, id := range []string{"other-account", "rotated-account"} {
					other, _ := m.GetByID(id)
					if blocked, _, _ := isAuthBlockedForModel(other, "model-x", state.UpdatedAt); blocked {
						t.Fatal("other credential blocked")
					}
				}
				m.MarkResult(ctx, Result{AuthID: a.ID, Provider: a.Provider, Model: "model-x", Success: true})
				a, _ = m.GetByID(a.ID)
				if blocked, _, _ := isAuthBlockedForModel(a, "model-x", state.UpdatedAt); blocked {
					t.Fatal("success did not clear denial")
				}
			})
		}
	}
}
