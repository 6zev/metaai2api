// Package openai serves the OpenAI-compatible HTTP surface of metaai2api.
//
// It owns the /v1 wire types and handlers only. Routing, middleware and admin
// endpoints live in internal/server, which mounts the handler returned by New.
package openai

import (
	"encoding/json"
	"fmt"
)

// Transport model names accepted from clients.
const (
	ModelThinkFast = "meta-think-fast"
	ModelThinkHard = "meta-think-hard"
)

// Upstream agent types the transport names map to.
const (
	AgentTypeThinkFast = "think_fast"
	AgentTypeThinkHard = "think_hard"
)

// ModelAgentType maps a transport model name to the upstream agent type.
// The second result is false for a name this server does not offer.
func ModelAgentType(model string) (string, bool) {
	switch model {
	case ModelThinkFast:
		return AgentTypeThinkFast, true
	case ModelThinkHard:
		return AgentTypeThinkHard, true
	default:
		return "", false
	}
}

// FlattenPrompt renders a conversation as one role-tagged prompt: one
// "role: content" line per message, in order, joined by newlines. The final
// line is always the latest message so an operator can read the prompt top to
// bottom. An empty conversation flattens to "" rather than panicking.
func FlattenPrompt(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	out := ""
	for i, m := range messages {
		if i > 0 {
			out += "\n"
		}
		content := ""
		if c := string(m.Content); c != "" {
			content = " " + c
		}
		out += m.Role + ":" + content
	}
	return out
}

// Content accepts either a JSON string or a JSON array of content parts
// ({"type":"text","text":"..."}); text parts are concatenated.
type Content string

// UnmarshalJSON implements json.Unmarshaler.
func (c *Content) UnmarshalJSON(raw []byte) error {
	if len(raw) == 0 || string(raw) == "null" {
		*c = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		*c = Content(s)
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return fmt.Errorf("content must be a string or an array of text parts")
	}
	var out string
	for _, p := range parts {
		if p.Type != "" && p.Type != "text" {
			return fmt.Errorf("unsupported content part type %q", p.Type)
		}
		out += p.Text
	}
	*c = Content(out)
	return nil
}

// MarshalJSON implements json.Marshaler.
func (c Content) MarshalJSON() ([]byte, error) { return json.Marshal(string(c)) }

// Message is one chat message on the wire. Content is flattened to a string on
// input and always emitted as a string on output.
type Message struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// ChatCompletionRequest is the body of POST /v1/chat/completions.
type ChatCompletionRequest struct {
	Model          string    `json:"model"`
	Messages       []Message `json:"messages"`
	Stream         bool      `json:"stream,omitempty"`
	ConversationID string    `json:"conversation_id,omitempty"`
}

// ChatCompletionResponse is a non-streaming completion.
type ChatCompletionResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []Completion `json:"choices"`
	Usage   *Usage       `json:"usage,omitempty"`
}

// Completion is one choice of a non-streaming completion.
type Completion struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatCompletionChunk is one SSE frame of a streaming completion.
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
}

// ChunkChoice is one choice of a streamed chunk.
type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

// ChunkDelta is the incremental content of a chunk.
type ChunkDelta struct {
	Role    string  `json:"role,omitempty"`
	Content Content `json:"content,omitempty"`
}

// Usage reports token accounting. meta.ai does not return real counts, so the
// handler omits usage entirely rather than inventing numbers.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Model is one entry of the /v1/models list.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelList is the body of GET /v1/models.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ErrorResponse is the OpenAI error envelope.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// APIError describes a failed request.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    any    `json:"code,omitempty"`
}

// Error types used in APIError.Type.
const (
	errTypeInvalidRequest = "invalid_request_error"
	errTypeAuth           = "authentication_error"
	errTypeServer         = "server_error"
	errTypeNotFound       = "not_found_error"
	errTypeNotImplemented = "not_implemented_error"
)

// sseError is the payload of an SSE error event emitted mid-stream.
type sseError struct {
	Error APIError `json:"error"`
}
