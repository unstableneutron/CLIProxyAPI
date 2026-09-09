package synthesizer

import (
	"bytes"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestPluginEnvironmentKeysAreRuntimeOnlyAndAccountIsolated(t *testing.T) {
	t.Setenv("CPA_TEST_PLUGIN_KEY_A", "test-account-a")
	t.Setenv("CPA_TEST_PLUGIN_KEY_B", "test-account-b")
	t.Setenv("CPA_TEST_PLUGIN_KEY_MISSING", "")
	var cfg config.Config
	err := yaml.Unmarshal([]byte(`plugins:
  enabled: true
  api-keys:
    - provider: commandcode
      api-key-env: CPA_TEST_PLUGIN_KEY_A
    - provider: commandcode
      api-key-env: CPA_TEST_PLUGIN_KEY_B
    - provider: other-plugin
      api-key-env: CPA_TEST_PLUGIN_KEY_A
    - provider: commandcode
      api-key-env: CPA_TEST_PLUGIN_KEY_MISSING
`), &cfg)
	if err != nil {
		t.Fatal(err)
	}
	synthesize := func() []*coreauth.Auth {
		t.Helper()
		auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: &cfg, Now: time.Unix(1, 0), IDGenerator: NewStableIDGenerator()})
		if err != nil {
			t.Fatal(err)
		}
		return auths
	}
	auths := synthesize()
	if len(auths) != 3 {
		t.Fatalf("got %d auths; missing environment key must not create one", len(auths))
	}
	for _, auth := range auths {
		if !coreauth.IsConfigAPIKeyAuth(auth) || auth.FileName != "" || auth.Metadata != nil || auth.Storage != nil {
			t.Fatal("environment auth must be config-owned and nonpersistent")
		}
	}
	if auths[0].ID == auths[1].ID || auths[0].ID == auths[2].ID {
		t.Fatal("credential and provider identities must be isolated")
	}
	if auths[0].ID != synthesize()[0].ID {
		t.Fatal("unchanged environment account identity must be stable")
	}
	t.Setenv("CPA_TEST_PLUGIN_KEY_A", "test-rotated-account")
	if auths[0].ID == synthesize()[0].ID {
		t.Fatal("rotated credentials must not inherit the previous account's health state")
	}
	serialized, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(serialized, []byte("test-account")) || bytes.Contains(serialized, []byte("test-rotated")) {
		t.Fatal("configuration serialization contains a resolved secret")
	}
	cfg.Plugins.Enabled = false
	if len(synthesize()) != 0 {
		t.Fatal("disabled plugins created environment auths")
	}
}
