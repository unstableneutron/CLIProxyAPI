package pluginhost

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRPCStartLoginForwardsOptionalMetadata(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata map[string]any
	}{
		{name: "omitted"},
		{name: "kiro variant", metadata: map[string]any{"login_method": "idc-authcode", "start_url": "https://example.awsapps.com/start", "region": "eu-west-1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &authStartCaptureClient{t: t}
			adapter := &rpcPluginAdapter{id: "auth-plugin", host: New(), client: client}
			_, errStart := adapter.StartLogin(context.Background(), pluginapi.AuthLoginStartRequest{Provider: "kiro", Metadata: test.metadata})
			if errStart != nil {
				t.Fatal(errStart)
			}
			if len(test.metadata) == 0 && client.request.Metadata != nil {
				t.Fatalf("RPC metadata = %#v, want nil", client.request.Metadata)
			}
			if len(test.metadata) > 0 && client.request.Metadata["login_method"] != "idc-authcode" {
				t.Fatalf("RPC metadata = %#v", client.request.Metadata)
			}
		})
	}
}

type authStartCaptureClient struct {
	t       *testing.T
	request rpcAuthLoginStartRequest
}

func (c *authStartCaptureClient) Call(_ context.Context, method string, request []byte) ([]byte, error) {
	if method != pluginabi.MethodAuthLoginStart {
		c.t.Fatalf("method = %q", method)
	}
	if err := json.Unmarshal(request, &c.request); err != nil {
		c.t.Fatal(err)
	}
	return marshalRPCResult(pluginapi.AuthLoginStartResponse{Provider: "kiro", URL: "https://login.example", State: "state"})
}

func (c *authStartCaptureClient) Shutdown() {}
