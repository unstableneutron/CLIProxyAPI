package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestOpenAICompatExecutor_resolveWireAPI(t *testing.T) {
	e := &OpenAICompatExecutor{provider: "test"}

	tests := []struct {
		name     string
		auth     *cliproxyauth.Auth
		expected string
	}{
		{
			name:     "nil auth",
			auth:     nil,
			expected: "chat",
		},
		{
			name:     "nil attributes",
			auth:     &cliproxyauth.Auth{},
			expected: "chat",
		},
		{
			name: "wire_api not set",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"base_url": "https://example.com",
				},
			},
			expected: "chat",
		},
		{
			name: "wire_api set to chat",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"wire_api": "chat",
				},
			},
			expected: "chat",
		},
		{
			name: "wire_api set to responses",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"wire_api": "responses",
				},
			},
			expected: "responses",
		},
		{
			name: "wire_api with whitespace",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"wire_api": "  responses  ",
				},
			},
			expected: "responses",
		},
		{
			name: "wire_api invalid value defaults to chat",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"wire_api": "invalid",
				},
			},
			expected: "chat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := e.resolveWireAPI(tt.auth)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestOpenAICompatExecutor_buildRequestURL(t *testing.T) {
	e := &OpenAICompatExecutor{provider: "test"}

	tests := []struct {
		name     string
		baseURL  string
		auth     *cliproxyauth.Auth
		expected string
	}{
		{
			name:     "OpenAI chat completions with nil auth",
			baseURL:  "https://api.openai.com/v1",
			auth:     nil,
			expected: "https://api.openai.com/v1/chat/completions",
		},
		{
			name:    "OpenRouter default chat endpoint",
			baseURL: "https://openrouter.ai/api/v1",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{},
			},
			expected: "https://openrouter.ai/api/v1/chat/completions",
		},
		{
			name:    "OpenAI responses endpoint",
			baseURL: "https://api.openai.com/v1",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"wire_api": "responses",
				},
			},
			expected: "https://api.openai.com/v1/responses",
		},
		{
			name:    "Azure OpenAI with trailing slash",
			baseURL: "https://my-resource.openai.azure.com/openai/deployments/gpt-4/",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"wire_api": "chat",
				},
			},
			expected: "https://my-resource.openai.azure.com/openai/deployments/gpt-4/chat/completions",
		},
		{
			name:    "Together AI chat endpoint",
			baseURL: "https://api.together.xyz/v1",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"wire_api": "chat",
				},
			},
			expected: "https://api.together.xyz/v1/chat/completions",
		},
		{
			name:    "Fireworks AI responses endpoint with trailing slash",
			baseURL: "https://api.fireworks.ai/inference/v1/",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"wire_api": "responses",
				},
			},
			expected: "https://api.fireworks.ai/inference/v1/responses",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := e.buildRequestURL(tt.baseURL, tt.auth)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}
