package quarantine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxAnswer caps what is read from the model's HTTP answer.
const maxAnswer = 1 << 20

// HTTPCompleter calls an OpenAI-compatible chat API (OpenAI, OpenRouter, Ollama
// under /v1) with net/http and no SDK. It never sends tools. Errors carry no
// text from the model or from the note.
type HTTPCompleter struct {
	BaseURL string // for example http://127.0.0.1:11434/v1
	APIKey  string // sent as a Bearer token only when set
	Client  *http.Client
}

// NewHTTPCompleter builds a completer; the timeout comes from the context of each call.
func NewHTTPCompleter(baseURL, apiKey string) *HTTPCompleter {
	return &HTTPCompleter{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, Client: &http.Client{}}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	Temperature    int           `json:"temperature"`
	MaxTokens      int           `json:"max_tokens"`
	ResponseFormat responseForm  `json:"response_format"`
}

type responseForm struct {
	Type       string     `json:"type"`
	JSONSchema schemaSpec `json:"json_schema"`
}

type schemaSpec struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete posts one chat request with temperature 0, at most 256 tokens and the
// closed schema as the response format.
func (c *HTTPCompleter) Complete(ctx context.Context, req Request) (Completion, error) {
	body, err := json.Marshal(chatRequest{
		Model:       req.Model,
		Messages:    []chatMessage{{Role: "system", Content: req.System}, {Role: "user", Content: req.User}},
		Temperature: 0,
		MaxTokens:   256,
		ResponseFormat: responseForm{Type: "json_schema",
			JSONSchema: schemaSpec{Name: req.SchemaID, Strict: true, Schema: req.Schema}},
	})
	if err != nil {
		return Completion{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Completion{}, errors.New("quarantine: bad base URL")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.Client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return Completion{}, ctx.Err()
		}
		return Completion{}, errors.New("quarantine: request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		if ctx.Err() != nil {
			return Completion{}, ctx.Err()
		}
		return Completion{}, errors.New("quarantine: read failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Completion{}, fmt.Errorf("quarantine: upstream status %d", resp.StatusCode)
	}
	var out chatResponse
	if json.Unmarshal(raw, &out) != nil || len(out.Choices) == 0 {
		return Completion{}, errors.New("quarantine: unreadable answer")
	}
	comp := Completion{Text: out.Choices[0].Message.Content}
	if out.Usage != nil {
		comp.PromptTokens, comp.CompletionTokens = out.Usage.PromptTokens, out.Usage.CompletionTokens
	}
	return comp, nil
}
