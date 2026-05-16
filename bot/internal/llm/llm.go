package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"llm-telegram-bot/internal/textutil"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string         `json:"type"`
	Function ToolDefinition `json:"function"`
}

type ToolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type StreamHandler interface {
	OnText(delta string)
	OnToolStart(name string)
}

// Client holds the connection to llama-server plus the sampling parameters
// applied to every Chat call. Fields are populated from config; defaults are
// Qwen's official sampling recipe.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	MaxTokens     int
	Temperature   float64
	TopP          float64
	TopK          int
	MinP          float64
	RepeatPenalty float64
}

func New(baseURL string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: 0}, // no overall timeout — streaming may run long
		// Defaults match Qwen 2.5 / Qwen 3; main.go overrides from config.
		MaxTokens:     800,
		Temperature:   0.7,
		TopP:          0.8,
		TopK:          20,
		MinP:          0.0,
		RepeatPenalty: 1.0,
	}
}

// WaitReady polls /health until 200 OK or the timeout elapses.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	probe := &http.Client{Timeout: 3 * time.Second}
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s", timeout)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/health", nil)
		resp, err := probe.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
	TopP        float64   `json:"top_p,omitempty"`
	TopK        int       `json:"top_k,omitempty"`
	MinP        float64   `json:"min_p,omitempty"`
	// llama.cpp extension (not OpenAI). 1.0 disables it; we set explicitly
	// because llama.cpp defaults to 1.1 which can suppress tool-call tokens.
	RepeatPenalty float64 `json:"repeat_penalty,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// Chat performs one streaming chat completion. Returns accumulated assistant
// text plus any tool calls the model wants to make.
func (c *Client) Chat(ctx context.Context, msgs []Message, tools []Tool, h StreamHandler) (string, []ToolCall, error) {
	body, err := json.Marshal(chatRequest{
		Model:         "local",
		Messages:      msgs,
		Tools:         tools,
		Stream:        true,
		MaxTokens:     c.MaxTokens,
		Temperature:   c.Temperature,
		TopP:          c.TopP,
		TopK:          c.TopK,
		MinP:          c.MinP,
		RepeatPenalty: c.RepeatPenalty,
	})
	if err != nil {
		return "", nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("llama-server %d: %s", resp.StatusCode, textutil.Ellipsize(string(b), 400))
	}

	var content strings.Builder
	calls := map[int]*ToolCall{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.Delta.Content != "" {
			content.WriteString(ch.Delta.Content)
			if h != nil {
				h.OnText(ch.Delta.Content)
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			cur := calls[tc.Index]
			if cur == nil {
				cur = &ToolCall{Type: "function"}
				calls[tc.Index] = cur
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Type != "" {
				cur.Type = tc.Type
			}
			if tc.Function.Name != "" {
				if cur.Function.Name == "" && h != nil {
					h.OnToolStart(tc.Function.Name)
				}
				cur.Function.Name += tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				cur.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, err
	}

	indices := make([]int, 0, len(calls))
	for i := range calls {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	ordered := make([]ToolCall, 0, len(indices))
	for _, i := range indices {
		ordered = append(ordered, *calls[i])
	}
	return content.String(), ordered, nil
}
