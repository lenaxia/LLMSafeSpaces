// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
)

func TestLLMExplainer_DisabledWhenBaseURLEmpty(t *testing.T) {
	t.Parallel()
	e := NewLLMExplainer(LLMExplainerConfig{})
	explanation, attr, err := e.Explain(context.Background(), "log tail", imagefactory.ResolvedValues{})
	require.NoError(t, err)
	assert.Equal(t, fallbackExplanation, explanation)
	assert.Empty(t, attr)
}

func TestLLMExplainer_DisabledWhenModelEmpty(t *testing.T) {
	t.Parallel()
	e := NewLLMExplainer(LLMExplainerConfig{BaseURL: "http://localhost:4000/v1"})
	explanation, attr, _ := e.Explain(context.Background(), "log tail", nil)
	assert.Equal(t, fallbackExplanation, explanation)
	assert.Empty(t, attr)
}

func TestLLMExplainer_HappyPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		var req chatCompletionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "test-model", req.Model)

		// Return a structured JSON explanation.
		inner := explainResponse{
			Explanation:         "apt package 'ffmpegx' does not exist in Debian bookworm repos",
			AttributedExtension: "ffmpegx",
		}
		innerJSON, _ := json.Marshal(inner)
		resp := chatCompletionResponse{}
		resp.Choices = []struct {
			Message chatMessage `json:"message"`
		}{{Message: chatMessage{Role: "assistant", Content: string(innerJSON)}}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := NewLLMExplainer(LLMExplainerConfig{
		BaseURL: srv.URL,
		Model:   "test-model",
		APIKey:  "test-key",
	})

	rv := imagefactory.ResolvedValues{
		"ffmpegx": {Type: imagefactory.ExtensionTypeApt, Value: "ffmpegx"},
	}
	explanation, attr, err := e.Explain(context.Background(), "E: Unable to locate package ffmpegx", rv)
	require.NoError(t, err)
	assert.Contains(t, explanation, "ffmpegx")
	assert.Contains(t, explanation, "does not exist")
	assert.Equal(t, "ffmpegx", attr)
}

func TestLLMExplainer_LLMDown_ReturnsFallback(t *testing.T) {
	t.Parallel()
	e := NewLLMExplainer(LLMExplainerConfig{
		BaseURL: "http://127.0.0.1:1", // unreachable
		Model:   "test-model",
	})
	explanation, attr, _ := e.Explain(context.Background(), "log tail", nil)
	assert.Equal(t, fallbackExplanation, explanation)
	assert.Empty(t, attr, "no attribution on LLM failure")
}

func TestLLMExplainer_LLMReturnsNonJSON_ReturnsRawContent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatCompletionResponse{}
		resp.Choices = []struct {
			Message chatMessage `json:"message"`
		}{{Message: chatMessage{Role: "assistant", Content: "The build failed because apt could not find the package."}}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := NewLLMExplainer(LLMExplainerConfig{BaseURL: srv.URL, Model: "m"})
	explanation, _, err := e.Explain(context.Background(), "log", nil)
	require.NoError(t, err)
	assert.Contains(t, explanation, "apt could not find")
}

func TestLLMExplainer_LLMReturnsEmptyExplanation_ReturnsFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner := explainResponse{Explanation: ""}
		innerJSON, _ := json.Marshal(inner)
		resp := chatCompletionResponse{}
		resp.Choices = []struct {
			Message chatMessage `json:"message"`
		}{{Message: chatMessage{Content: string(innerJSON)}}}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := NewLLMExplainer(LLMExplainerConfig{BaseURL: srv.URL, Model: "m"})
	explanation, _, _ := e.Explain(context.Background(), "log", nil)
	assert.Equal(t, fallbackExplanation, explanation)
}

func TestLLMExplainer_LLMReturns500_ReturnsFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := NewLLMExplainer(LLMExplainerConfig{BaseURL: srv.URL, Model: "m"})
	explanation, _, _ := e.Explain(context.Background(), "log", nil)
	assert.Equal(t, fallbackExplanation, explanation)
}

func TestLLMExplainer_LLMReturnsEmptyChoices_ReturnsFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(chatCompletionResponse{})
	}))
	defer srv.Close()

	e := NewLLMExplainer(LLMExplainerConfig{BaseURL: srv.URL, Model: "m"})
	explanation, _, _ := e.Explain(context.Background(), "log", nil)
	assert.Equal(t, fallbackExplanation, explanation)
}

func TestBuildExplainPrompt_IncludesExtensionsAndLog(t *testing.T) {
	t.Parallel()
	rv := imagefactory.ResolvedValues{
		"ffmpeg":    {Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg"},
		"python313": {Type: imagefactory.ExtensionTypeMise, Value: "python@3.13"},
	}
	prompt := buildExplainPrompt("apt: 404 not found", rv)
	assert.Contains(t, prompt, "ffmpeg")
	assert.Contains(t, prompt, "python@3.13")
	assert.Contains(t, prompt, "apt: 404 not found")
	assert.Contains(t, prompt, "Explain why this build failed")
}
