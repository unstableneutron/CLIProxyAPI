package pluginhost

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestExecutorAdapterPrepareAppliesPayloadDefaultsAndOverrides(t *testing.T) {
	host := New()
	host.runtimeConfig = &config.Config{Payload: config.PayloadConfig{
		Default: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "native-model", Protocol: "openai"}},
			Params: map[string]any{"temperature": 0.25},
		}},
		Override: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{Name: "native-model", Protocol: "openai"}},
			Params: map[string]any{"top_p": 0.4},
		}},
	}}
	adapter := &executorAdapter{
		host:          host,
		provider:      "openai",
		inputFormats:  []sdktranslator.Format{sdktranslator.FormatOpenAI},
		outputFormats: []sdktranslator.Format{sdktranslator.FormatOpenAI},
	}

	for _, test := range []struct {
		name            string
		payload         []byte
		wantTemperature float64
	}{
		{name: "missing gets default", payload: []byte(`{"model":"native-model","messages":[],"top_p":0.9}`), wantTemperature: 0.25},
		{name: "explicit survives default", payload: []byte(`{"model":"native-model","messages":[],"temperature":0.7,"top_p":0.9}`), wantTemperature: 0.7},
	} {
		t.Run(test.name, func(t *testing.T) {
			originalPayload := bytes.Clone(test.payload)
			originalRequest := bytes.Clone(test.payload)
			headers := http.Header{"X-Test": []string{"original"}}
			metadata := map[string]any{"nested": map[string]any{"value": "original"}}
			prepared, errPrepare := adapter.prepareExecutorCall(coreexecutor.Request{
				Model:    "native-model",
				Format:   sdktranslator.FormatOpenAI,
				Payload:  test.payload,
				Metadata: metadata,
			}, coreexecutor.Options{
				SourceFormat:    sdktranslator.FormatOpenAI,
				ResponseFormat:  sdktranslator.FormatOpenAI,
				OriginalRequest: originalRequest,
				Headers:         headers,
			})
			if errPrepare != nil {
				t.Fatalf("prepareExecutorCall() error = %v", errPrepare)
			}
			if got := gjson.GetBytes(prepared.req.Payload, "temperature").Float(); got != test.wantTemperature {
				t.Fatalf("temperature = %v, want %v; payload=%s", got, test.wantTemperature, prepared.req.Payload)
			}
			if got := gjson.GetBytes(prepared.req.Payload, "top_p").Float(); got != 0.4 {
				t.Fatalf("top_p = %v, want forced override 0.4; payload=%s", got, prepared.req.Payload)
			}
			if !bytes.Equal(test.payload, originalPayload) || !bytes.Equal(originalRequest, originalPayload) {
				t.Fatalf("input payload bytes mutated: payload=%s original=%s", test.payload, originalRequest)
			}
			if headers.Get("X-Test") != "original" || metadata["nested"].(map[string]any)["value"] != "original" {
				t.Fatalf("input maps mutated: headers=%v metadata=%v", headers, metadata)
			}
		})
	}
}

func TestExecutorAdapterPrepareUsesAliasHeadersAndRequestPath(t *testing.T) {
	host := New()
	host.runtimeConfig = &config.Config{
		SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationChat},
		Payload: config.PayloadConfig{Default: []config.PayloadRule{{
			Models: []config.PayloadModelRule{{
				Name:         "client-alias",
				Protocol:     "openai",
				FromProtocol: "openai",
				Headers:      map[string]string{"X-Tenant": "gold-*"},
			}},
			Params: map[string]any{"temperature": 0.25},
		}}},
	}
	adapter := &executorAdapter{
		host:          host,
		provider:      "openai",
		inputFormats:  []sdktranslator.Format{sdktranslator.FormatOpenAI},
		outputFormats: []sdktranslator.Format{sdktranslator.FormatOpenAI},
	}
	payload := []byte(`{"model":"upstream-model","messages":[],"tools":[{"type":"image_generation"},{"type":"function","function":{"name":"lookup"}}]}`)

	for _, test := range []struct {
		name           string
		path           string
		tenant         string
		wantTemp       float64
		wantImageCount int64
	}{
		{name: "alias and header match", path: "/v1/chat/completions", tenant: "gold-team", wantTemp: 0.25, wantImageCount: 0},
		{name: "images path preserves image tool", path: "/v1/images/generations", tenant: "gold-team", wantTemp: 0.25, wantImageCount: 1},
		{name: "header mismatch skips rule", path: "/v1/chat/completions", tenant: "silver-team", wantImageCount: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := http.Header{"X-Tenant": []string{test.tenant}}
			metadata := map[string]any{
				coreexecutor.RequestedModelMetadataKey: "client-alias",
				coreexecutor.RequestPathMetadataKey:    test.path,
			}
			metadataBefore := map[string]any{
				coreexecutor.RequestedModelMetadataKey: "client-alias",
				coreexecutor.RequestPathMetadataKey:    test.path,
			}
			prepared, errPrepare := adapter.prepareExecutorCall(coreexecutor.Request{
				Model:   "upstream-model",
				Format:  sdktranslator.FormatOpenAI,
				Payload: payload,
			}, coreexecutor.Options{
				SourceFormat: sdktranslator.FormatOpenAI,
				Headers:      headers,
				Metadata:     metadata,
			})
			if errPrepare != nil {
				t.Fatalf("prepareExecutorCall() error = %v", errPrepare)
			}
			if got := gjson.GetBytes(prepared.req.Payload, "temperature").Float(); got != test.wantTemp {
				t.Fatalf("alias/header-gated temperature = %v, want %v; payload=%s", got, test.wantTemp, prepared.req.Payload)
			}
			var imageCount int64
			gjson.GetBytes(prepared.req.Payload, "tools").ForEach(func(_, tool gjson.Result) bool {
				if tool.Get("type").String() == "image_generation" {
					imageCount++
				}
				return true
			})
			if imageCount != test.wantImageCount {
				t.Fatalf("image tool count = %d, want %d; payload=%s", imageCount, test.wantImageCount, prepared.req.Payload)
			}
			if headers.Get("X-Tenant") != test.tenant || !reflect.DeepEqual(metadata, metadataBefore) {
				t.Fatalf("matching inputs mutated: headers=%v metadata=%v", headers, metadata)
			}
		})
	}
}

func TestExecutorAdapterPrepareTranslatesOriginalForCrossFormatDefaults(t *testing.T) {
	host := New()
	host.runtimeConfig = &config.Config{Payload: config.PayloadConfig{Default: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "native-model", Protocol: "claude", FromProtocol: "openai"}},
		Params: map[string]any{"max_tokens": 123},
	}}}}
	adapter := &executorAdapter{
		host:          host,
		provider:      "claude",
		inputFormats:  []sdktranslator.Format{sdktranslator.FormatClaude},
		outputFormats: []sdktranslator.Format{sdktranslator.FormatClaude},
	}
	payload := []byte(`{"model":"native-model","messages":[{"role":"user","content":"hi"}]}`)
	original := []byte(`{"model":"native-model","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":700}`)

	prepared, errPrepare := adapter.prepareExecutorCall(coreexecutor.Request{
		Model:   "native-model",
		Format:  sdktranslator.FormatOpenAI,
		Payload: payload,
	}, coreexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAI,
		OriginalRequest: original,
	})
	if errPrepare != nil {
		t.Fatalf("prepareExecutorCall() error = %v", errPrepare)
	}
	if got := gjson.GetBytes(prepared.req.Payload, "max_tokens").Int(); got != 32000 {
		t.Fatalf("max_tokens = %d, want translator value 32000 because original translated max_tokens suppresses configured default; payload=%s", got, prepared.req.Payload)
	}
	if !bytes.Contains(prepared.req.Payload, []byte(`"messages"`)) {
		t.Fatalf("request was not translated to Claude: %s", prepared.req.Payload)
	}
	if !bytes.Equal(payload, []byte(`{"model":"native-model","messages":[{"role":"user","content":"hi"}]}`)) ||
		!bytes.Equal(original, []byte(`{"model":"native-model","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":700}`)) {
		t.Fatalf("cross-format inputs mutated: payload=%s original=%s", payload, original)
	}
}

func TestExecutorAdapterPrepareAppliesCanonicalThinkingAndRejectsInvalidLevel(t *testing.T) {
	const (
		clientID = "pluginhost-native-thinking-test-client"
		modelID  = "pluginhost-native-thinking-test-model"
	)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(clientID, "openai", []*registry.ModelInfo{{
		ID:       modelID,
		Type:     "openai",
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}},
	}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(clientID) })

	var calls int
	host := New()
	adapter := newCurrentExecutorAdapterForTest(host, "thinking-executor", &fakeExecutor{
		execute: func(_ context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
			calls++
			if req.Model != modelID {
				t.Fatalf("callback model = %q, want base model %q", req.Model, modelID)
			}
			if got := gjson.GetBytes(req.Payload, "reasoning_effort").String(); got != "high" {
				t.Fatalf("reasoning_effort = %q, want high; payload=%s", got, req.Payload)
			}
			return pluginapi.ExecutorResponse{Payload: []byte(`{}`)}, nil
		},
	}, []sdktranslator.Format{sdktranslator.FormatOpenAI}, []sdktranslator.Format{sdktranslator.FormatOpenAI})
	adapter.provider = "openai"

	_, errValid := adapter.Execute(context.Background(), nil, coreexecutor.Request{
		Model:   modelID + "(high)",
		Format:  sdktranslator.FormatOpenAI,
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if errValid != nil {
		t.Fatalf("Execute() valid suffix error = %v", errValid)
	}
	if calls != 1 {
		t.Fatalf("callback calls after valid suffix = %d, want 1", calls)
	}

	_, errInvalid := adapter.Execute(context.Background(), nil, coreexecutor.Request{
		Model:   modelID + "(max)",
		Format:  sdktranslator.FormatOpenAI,
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if errInvalid == nil || !strings.Contains(errInvalid.Error(), `level "max" not supported`) {
		t.Fatalf("Execute() invalid suffix error = %v, want unsupported max level", errInvalid)
	}
	if calls != 1 {
		t.Fatalf("callback calls after invalid suffix = %d, want unchanged 1", calls)
	}
}

func TestExecutorAdapterAllExecutionModesUsePreparedPayload(t *testing.T) {
	host := New()
	host.runtimeConfig = &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "native-model", Protocol: "openai"}},
		Params: map[string]any{"temperature": 0.25},
	}}}}
	seen := make(map[string]float64)
	check := func(mode string, req pluginapi.ExecutorRequest) {
		seen[mode] = gjson.GetBytes(req.Payload, "temperature").Float()
	}
	adapter := newCurrentExecutorAdapterForTest(host, "prepared-payload-executor", &fakeExecutor{
		execute: func(_ context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
			check("execute", req)
			return pluginapi.ExecutorResponse{Payload: []byte(`{}`)}, nil
		},
		executeStream: func(_ context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
			check("stream", req)
			chunks := make(chan pluginapi.ExecutorStreamChunk)
			close(chunks)
			return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
		},
		countTokens: func(_ context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
			check("count", req)
			return pluginapi.ExecutorResponse{Payload: []byte(`{}`)}, nil
		},
	}, []sdktranslator.Format{sdktranslator.FormatOpenAI}, []sdktranslator.Format{sdktranslator.FormatOpenAI})
	adapter.provider = "openai"
	req := coreexecutor.Request{
		Model:   "native-model",
		Format:  sdktranslator.FormatOpenAI,
		Payload: []byte(`{"model":"native-model","messages":[],"temperature":0.9}`),
	}
	opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}

	if _, errExecute := adapter.Execute(context.Background(), nil, req, opts); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	stream, errStream := adapter.ExecuteStream(context.Background(), nil, req, opts)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	for range stream.Chunks {
	}
	if _, errCount := adapter.CountTokens(context.Background(), nil, req, opts); errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	for _, mode := range []string{"execute", "stream", "count"} {
		if got := seen[mode]; got != 0.25 {
			t.Fatalf("%s callback temperature = %v, want 0.25; seen=%v", mode, got, seen)
		}
	}
}
