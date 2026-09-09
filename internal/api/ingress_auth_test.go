package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

type nilResultAccessProvider struct{}

func (nilResultAccessProvider) Identifier() string { return "nil-result" }

func (nilResultAccessProvider) Authenticate(context.Context, *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
	return nil, nil
}

func TestAuthenticatePluginIngressRejectsMissingResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name      string
		providers []sdkaccess.Provider
	}{
		{name: "empty provider manager"},
		{name: "provider returns nil result and nil error", providers: []sdkaccess.Provider{nilResultAccessProvider{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			manager := sdkaccess.NewManager()
			manager.SetProviders(tc.providers)
			server := &Server{accessManager: manager}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/backend-api/models", nil)

			if server.authenticatePluginIngress(ctx) {
				t.Fatal("authenticatePluginIngress() = true, want false")
			}
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
		})
	}
}
