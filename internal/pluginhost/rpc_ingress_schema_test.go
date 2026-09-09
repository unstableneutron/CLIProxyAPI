package pluginhost

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
)

func TestRPCIngressRequiresSchemaEight(t *testing.T) {
	for _, tc := range []struct {
		name    string
		schema  uint32
		ingress bool
		wantErr bool
	}{
		{"legacy without ingress", 0, false, false},
		{"legacy with ingress", 0, true, true},
		{"schema seven without ingress", 7, false, false},
		{"schema seven with ingress", 7, true, true},
		{"schema eight with ingress", 8, true, false},
		{"future schema", 9, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, errMarshal := marshalRPCResult(rpcRegistration{
				SchemaVersion: tc.schema,
				Capabilities:  rpcCapabilities{IngressProxy: tc.ingress},
			})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			plugin, errRegister := registerRPCPlugin(context.Background(), New(), "fixture", staticEnvelopePluginClient{raw: raw}, pluginabi.MethodPluginRegister, nil)
			if (errRegister != nil) != tc.wantErr {
				t.Fatalf("registration error = %v, want error %v", errRegister, tc.wantErr)
			}
			if !tc.wantErr && (plugin.Capabilities.IngressProxy != nil) != tc.ingress {
				t.Fatal("ingress capability did not match registration")
			}
		})
	}
}
