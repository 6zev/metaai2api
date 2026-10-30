package openai_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	backend "github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/meta/mock"
	"github.com/me/metaai2api/internal/openai"
)

const (
	testAPIKey    = "test-api-key"
	credsRequired = "invalid or missing API token"
)

// newAPI mounts the handlers on an httptest server.
func newAPI(t *testing.T, b backend.ChatBackend, cfg openai.Options) *httptest.Server {
	t.Helper()
	cfg.Backend = b
	h, err := openai.New(cfg)
	if err != nil {
		t.Fatalf("openai.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func get(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}
}

// errorBody is the OpenAI error envelope.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, want, raw)
	}
}

// assertOpenAIError asserts status and that the body is an OpenAI error
// envelope whose message contains sub.
func assertOpenAIError(t *testing.T, resp *http.Response, wantStatus int, sub string) {
	t.Helper()
	assertStatus(t, resp, wantStatus)
	var eb errorBody
	decodeBody(t, resp, &eb)
	if eb.Error.Message == "" {
		t.Fatalf("empty error message in envelope")
	}
	if sub != "" && !strings.Contains(eb.Error.Message, sub) {
		t.Fatalf("error message %q does not contain %q", eb.Error.Message, sub)
	}
}

// ---- completion (non-streaming) ----

func TestChatCompletionNonStream(t *testing.T) {
	b := newGateBackend("hello", " ", "world")
	close(b.release)
	srv := newAPI(t, b, openai.Options{APIToken: testAPIKey})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", testAPIKey, `{
		"model": "meta-think-fast",
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	assertStatus(t, resp, http.StatusOK)

	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int `json:"index"`
			Message      struct{ Role, Content string }
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	decodeBody(t, resp, &out)
	if out.Object != "chat.completion" {
		t.Fatalf("object = %q", out.Object)
	}
	if out.Model != "meta-think-fast" {
		t.Fatalf("model = %q, want echo of request model", out.Model)
	}
	if out.ID == "" || !strings.HasPrefix(out.ID, "chatcmpl-") {
		t.Fatalf("id = %q", out.ID)
	}
	if len(out.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(out.Choices))
	}
	c := out.Choices[0]
	if c.Message.Role != "assistant" || c.Message.Content != "hello world" {
		t.Fatalf("choice = %+v", c)
	}
	if c.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", c.FinishReason)
	}
}

func TestChatCompletionNonStreamMapsModelAndFlattensPrompt(t *testing.T) {
	b := newGateBackend("ok")
	close(b.release)
	srv := newAPI(t, b, openai.Options{})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{
		"model": "meta-think-hard",
		"messages": [
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "first"},
			{"role": "assistant", "content": "reply"},
			{"role": "user", "content": "second"}
		]
	}`)
	assertStatus(t, resp, http.StatusOK)

	var req backend.ChatRequest
	select {
	case req = <-b.requests:
	case <-time.After(2 * time.Second):
		t.Fatal("backend never received a request")
	}
	if req.Model != "think_hard" {
		t.Fatalf("model = %q, want think_hard", req.Model)
	}
	if req.Stream {
		t.Fatal("non-streaming request reached backend with Stream=true")
	}

	// The flattening contract: one role-tagged line per message, in order.
	// The last user message must still be discoverable intact.
	if len(req.Messages) != 1 {
		t.Fatalf("backend messages = %d, want the flattened prompt as a single message", len(req.Messages))
	}
	got := req.Messages[0]
	if got.Role != "user" {
		t.Fatalf("flattened role = %q, want user", got.Role)
	}
	for _, want := range []string{"be terse", "first", "reply", "second"} {
		if !strings.Contains(got.Content, want) {
			t.Fatalf("flattened prompt %q missing %q", got.Content, want)
		}
	}
	// Order matters: system prompt first, latest user turn last.
	if strings.LastIndex(got.Content, "second") < strings.LastIndex(got.Content, "first") {
		t.Fatalf("flattened prompt lost message order: %q", got.Content)
	}
	if !strings.HasSuffix(got.Content, "second") {
		t.Fatalf("flattened prompt should end with the latest message: %q", got.Content)
	}
}

func TestPromptFlatteningRoles(t *testing.T) {
	msg := func(role, content string) openai.Message {
		return openai.Message{Role: role, Content: openai.Content(content)}
	}
	cases := []struct {
		name     string
		messages []openai.Message
		want     string
	}{
		{
			name:     "two turns",
			messages: []openai.Message{msg("user", "hi"), msg("assistant", "yo"), msg("user", "bye")},
			want:     "user: hi\nassistant: yo\nuser: bye",
		},
		{
			name:     "system folded into user turn",
			messages: []openai.Message{msg("system", "be brief"), msg("user", "q")},
			want:     "system: be brief\nuser: q",
		},
		{
			name:     "empty content keeps the role tag",
			messages: []openai.Message{msg("user", "")},
			want:     "user:",
		},
		{
			name:     "empty conversation",
			messages: nil,
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openai.FlattenPrompt(tc.messages); got != tc.want {
				t.Fatalf("FlattenPrompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmptyMessagesDoesNotPanic(t *testing.T) {
	b := newGateBackend("empty")
	close(b.release)
	srv := newAPI(t, b, openai.Options{})

	resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-fast","messages":[]}`)
	assertOpenAIError(t, resp, http.StatusBadRequest, "messages")

	// null messages must not panic either
	resp2 := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-fast","messages":null}`)
	assertOpenAIError(t, resp2, http.StatusBadRequest, "messages")
}

func TestMalformedJSON400(t *testing.T) {
	srv := newAPI(t, mock.New(), openai.Options{})
	resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":`)
	assertOpenAIError(t, resp, http.StatusBadRequest, "")
}

// ---- model mapping ----

func TestUnknownModel400(t *testing.T) {
	b := newGateBackend("x")
	close(b.release)
	srv := newAPI(t, b, openai.Options{})

	for _, model := range []string{"gpt-4o", "meta-think-faster", "think_fast", "Meta-Think-Fast"} {
		t.Run(model, func(t *testing.T) {
			resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
			assertOpenAIError(t, resp, http.StatusBadRequest, model)
		})
	}
	select {
	case req := <-b.requests:
		t.Fatalf("unknown model reached the backend: %+v", req)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestModelMapping(t *testing.T) {
	cases := map[string]string{
		"meta-think-fast": "think_fast",
		"meta-think-hard": "think_hard",
	}
	for model, want := range cases {
		got, ok := openai.ModelAgentType(model)
		if !ok || got != want {
			t.Fatalf("ModelAgentType(%q) = %q, %v; want %q, true", model, got, ok, want)
		}
	}
	if _, ok := openai.ModelAgentType("nope"); ok {
		t.Fatal("ModelAgentType accepted an unknown model")
	}
}

// ---- auth ----

func TestAuthEnforcedWhenTokenSet(t *testing.T) {
	b := newGateBackend("x")
	close(b.release)
	srv := newAPI(t, b, openai.Options{APIToken: testAPIKey})

	body := `{"model":"meta-think-fast","messages":[{"role":"user","content":"hi"}]}`
	for _, tok := range []string{"", "wrong", testAPIKey + "x"} {
		resp := postJSON(t, srv.URL+"/v1/chat/completions", tok, body)
		assertOpenAIError(t, resp, http.StatusUnauthorized, credsRequired)
	}
	assertOpenAIError(t, get(t, srv.URL+"/v1/models", ""), http.StatusUnauthorized, credsRequired)
	assertOpenAIError(t, get(t, srv.URL+"/v1/models", "wrong"), http.StatusUnauthorized, credsRequired)
	assertOpenAIError(t, postJSON(t, srv.URL+"/v1/images/generations", "", `{}`), http.StatusUnauthorized, credsRequired)

	resp := postJSON(t, srv.URL+"/v1/chat/completions", testAPIKey, body)
	assertStatus(t, resp, http.StatusOK)
}

func TestAuthOpenWhenTokenUnset(t *testing.T) {
	b := newGateBackend("x")
	close(b.release)
	srv := newAPI(t, b, openai.Options{})

	assertStatus(t, postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-fast","messages":[{"role":"user","content":"hi"}]}`), http.StatusOK)
	assertStatus(t, get(t, srv.URL+"/v1/models", ""), http.StatusOK)
}

func TestAuthAcceptsCaseInsensitiveScheme(t *testing.T) {
	b := newGateBackend("x")
	close(b.release)
	srv := newAPI(t, b, openai.Options{APIToken: testAPIKey})

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "bearer "+testAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	assertStatus(t, resp, http.StatusOK)
}

// ---- models ----

func TestModels(t *testing.T) {
	srv := newAPI(t, mock.New(), openai.Options{})
	resp := get(t, srv.URL+"/v1/models", "")
	assertStatus(t, resp, http.StatusOK)

	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int64  `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	decodeBody(t, resp, &out)
	if out.Object != "list" {
		t.Fatalf("object = %q, want list", out.Object)
	}
	ids := map[string]bool{}
	for _, m := range out.Data {
		ids[m.ID] = true
		if m.Object != "model" {
			t.Fatalf("model %q object = %q", m.ID, m.Object)
		}
	}
	for _, want := range []string{"meta-think-fast", "meta-think-hard"} {
		if !ids[want] {
			t.Fatalf("models list missing %q: %+v", want, ids)
		}
	}
}

// ---- images ----

func TestImages501(t *testing.T) {
	srv := newAPI(t, mock.New(), openai.Options{})
	resp := postJSON(t, srv.URL+"/v1/images/generations", "", `{"prompt":"a cat","model":"dall-e-3"}`)
	assertOpenAIError(t, resp, http.StatusNotImplemented, "image")
}

// ---- SSE ----

func TestSSEFraming(t *testing.T) {
	b := newGateBackend("hel", "lo ", "world")
	srv := newAPI(t, b, openai.Options{})

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"meta-think-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	assertStatus(t, resp, http.StatusOK)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", resp.Header.Get("X-Accel-Buffering"))
	}

	// The gate backend has emitted exactly one delta ("hel") and is blocked.
	// Reading the role chunk and then the content chunk now proves the handler
	// flushed instead of buffering until the upstream finished.
	br := bufio.NewReader(resp.Body)
	var chunk struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	// firstContent returns the first event that carries delta content.
	firstContent := func() string {
		deadline := time.Now().Add(2 * time.Second)
		for {
			ev, err := readEvent(t, br, time.Until(deadline))
			if err != nil {
				t.Fatalf("chunk not flushed before upstream finished: %v", err)
			}
			if !strings.HasPrefix(ev, "data: ") {
				t.Fatalf("chunk %q does not start with \"data: \"", ev)
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "data: ")), &chunk); err != nil {
				t.Fatalf("decode chunk %q: %v", ev, err)
			}
			if chunk.Object != "chat.completion.chunk" {
				t.Fatalf("chunk object = %q", chunk.Object)
			}
			if chunk.Model != "meta-think-fast" {
				t.Fatalf("chunk model = %q", chunk.Model)
			}
			if len(chunk.Choices) != 1 {
				t.Fatalf("chunk choices = %d", len(chunk.Choices))
			}
			if chunk.Choices[0].Delta.Content == "" {
				continue
			}
			return chunk.Choices[0].Delta.Content
		}
	}
	if c := firstContent(); c != "hel" {
		t.Fatalf("first flushed delta content = %q, want hel", c)
	}

	// Nothing else may arrive while the upstream is still blocked. The probe
	// must own every read until it returns, or it would steal bytes from the
	// events that follow.
	nextEvent := make(chan string, 1)
	go func() {
		line, err := readEvent(t, br, 2*time.Second)
		if err != nil {
			nextEvent <- "ERR: " + err.Error()
			return
		}
		nextEvent <- line
	}()
	select {
	case line := <-nextEvent:
		t.Fatalf("received %q while upstream was still blocked", line)
	case <-time.After(150 * time.Millisecond):
	}

	close(b.release)

	var content strings.Builder
	content.WriteString("hel")
	var sawFinish bool
	deadline := time.Now().Add(5 * time.Second)
	// The already-running probe reads the next event.
	line := <-nextEvent
	var events []string
	for {
		if strings.HasPrefix(line, "ERR: ") {
			t.Fatalf("read event: %s", line)
		}
		events = append(events, line)
		if line == "data: [DONE]" {
			break
		}
		var c struct {
			Choices []struct {
				Delta        struct{ Content string }
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &c); err != nil {
			t.Fatalf("decode chunk %q: %v", line, err)
		}
		if len(c.Choices) == 1 {
			content.WriteString(c.Choices[0].Delta.Content)
			if c.Choices[0].FinishReason != nil {
				if *c.Choices[0].FinishReason != "stop" {
					t.Fatalf("finish_reason = %q", *c.Choices[0].FinishReason)
				}
				sawFinish = true
			}
		}
		var err error
		line, err = readEvent(t, br, time.Until(deadline))
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
	}
	if !sawFinish {
		t.Fatal("no chunk carried finish_reason \"stop\"")
	}
	if got := content.String(); got != "hello world" {
		t.Fatalf("streamed content = %q, want %q", got, "hello world")
	}
	if strings.Count(strings.Join(events, "\n"), "data: [DONE]") != 1 {
		t.Fatalf("want exactly one [DONE] terminator:\n%s", strings.Join(events, "\n"))
	}
	if len(events) != 4 {
		t.Fatalf("events = %d (%q), want \"lo \", world, finish, [DONE]", len(events), events)
	}
	if !strings.Contains(events[len(events)-2], `"finish_reason":"stop"`) {
		t.Fatalf("second to last event is not the finish chunk: %q", events[len(events)-2])
	}
}

// readEvent reads one SSE "data:" event and consumes its blank separator.
func readEvent(t *testing.T, br *bufio.Reader, timeout time.Duration) (string, error) {
	t.Helper()
	if timeout <= 0 {
		timeout = time.Second
	}
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- res{sb.String(), err}
				return
			}
			if line == "\n" || line == "\r\n" {
				ch <- res{sb.String(), nil}
				return
			}
			sb.WriteString(strings.TrimRight(line, "\r\n"))
		}
	}()
	select {
	case r := <-ch:
		return r.line, r.err
	case <-time.After(timeout):
		return "", errors.New("timeout waiting for SSE event")
	}
}

func TestSSEMultipleChunksAreSeparateEvents(t *testing.T) {
	b := newGateBackend("a")
	close(b.release)
	srv := newAPI(t, b, openai.Options{})
	resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-hard","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	assertStatus(t, resp, http.StatusOK)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with the DONE terminator: %q", body)
	}
	if strings.Count(body, "data:") < 3 {
		t.Fatalf("expected role+content+done chunks, got %q", body)
	}
	for _, ev := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if !strings.HasPrefix(ev, "data: ") {
			t.Fatalf("event %q lacks the data: prefix", ev)
		}
	}
}

// ---- backend error mapping ----

func TestStreamErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		sub    string
	}{
		{backend.ErrSessionExpired, http.StatusUnauthorized, "session expired"},
		{wrappedErr(backend.ErrSessionExpired), http.StatusUnauthorized, "session expired"},
		{backend.ErrUnavailable, http.StatusServiceUnavailable, ""},
		{backend.ErrNotImplemented, http.StatusNotImplemented, ""},
		{backend.ErrUpstreamProtocol, http.StatusBadGateway, ""},
		{errGateTest, http.StatusInternalServerError, "gate"},
	}
	for i, tc := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			srv := newAPI(t, &emitErrBackend{err: tc.err}, openai.Options{})
			resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			// The partial delta is flushed before the terminal error, so the
			// status line can no longer be changed. The contract: the stream
			// carries the partial text, one error event, then [DONE].
			assertStatus(t, resp, http.StatusOK)
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			events := splitEvents(string(raw))
			if len(events) < 2 {
				t.Fatalf("stream too short: %q", raw)
			}
			if events[len(events)-1] != "data: [DONE]" {
				t.Fatalf("last event = %q, want data: [DONE]", events[len(events)-1])
			}
			var sawErr bool
			for _, ev := range events[:len(events)-1] {
				var payload struct {
					Error *struct {
						Message string `json:"message"`
						Type    string `json:"type"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "data: ")), &payload); err != nil {
					t.Fatalf("decode event %q: %v", ev, err)
				}
				if payload.Error != nil {
					sawErr = true
					if payload.Error.Message == "" {
						t.Fatalf("error event has an empty message: %q", ev)
					}
					if tc.sub != "" && !strings.Contains(payload.Error.Message, tc.sub) {
						t.Fatalf("error message %q missing %q", payload.Error.Message, tc.sub)
					}
				}
			}
			if !sawErr {
				t.Fatalf("no error event in stream: %q", raw)
			}
		})
	}
}

// splitEvents returns the non-empty events of an SSE body.
func splitEvents(body string) []string {
	var out []string
	for _, ev := range strings.Split(body, "\n\n") {
		ev = strings.TrimSpace(ev)
		if ev != "" {
			out = append(out, ev)
		}
	}
	return out
}

func wrappedErr(err error) error {
	return fmt.Errorf("upstream: %w", err)
}

func TestCompleteErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		sub    string
	}{
		{backend.ErrSessionExpired, http.StatusUnauthorized, "session expired"},
		{backend.ErrUnavailable, http.StatusServiceUnavailable, ""},
		{backend.ErrNotImplemented, http.StatusNotImplemented, ""},
		{backend.ErrUpstreamProtocol, http.StatusBadGateway, ""},
		{errGateTest, http.StatusInternalServerError, "gate"},
	}
	for i, tc := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			b := newGateBackend("x")
			b.setErr(tc.err)
			srv := newAPI(t, b, openai.Options{})
			resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-fast","messages":[{"role":"user","content":"hi"}]}`)
			assertOpenAIError(t, resp, tc.status, tc.sub)
		})
	}
}

func TestWrappedErrorStillMapped(t *testing.T) {
	b := newGateBackend("x")
	b.setErr(wrappedErr(backend.ErrSessionExpired))
	srv := newAPI(t, b, openai.Options{})
	resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-fast","messages":[{"role":"user","content":"hi"}]}`)
	assertOpenAIError(t, resp, http.StatusUnauthorized, "session expired")
}

// ---- misc ----

func TestStreamWithMockBackend(t *testing.T) {
	srv := newAPI(t, mock.New(mock.WithChunks(4), mock.WithSleep(time.Millisecond)), openai.Options{})
	resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	assertStatus(t, resp, http.StatusOK)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	for _, ev := range splitEvents(string(raw)) {
		if ev == "data: [DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct{ Content string }
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "data: ")), &c); err != nil {
			t.Fatalf("decode %q: %v", ev, err)
		}
		if len(c.Choices) == 1 {
			content.WriteString(c.Choices[0].Delta.Content)
		}
	}
	if !strings.Contains(content.String(), "hi") {
		t.Fatalf("mock stream did not echo the prompt: %q", content.String())
	}
}

func TestNewRejectsNilBackend(t *testing.T) {
	if _, err := openai.New(openai.Options{}); err == nil {
		t.Fatal("New(nil backend) = nil error, want error")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	srv := newAPI(t, mock.New(), openai.Options{})
	resp := postJSON(t, srv.URL+"/v1/models", "", `{}`)
	assertOpenAIError(t, resp, http.StatusMethodNotAllowed, "")
}

func TestUnknownV1Path404(t *testing.T) {
	srv := newAPI(t, mock.New(), openai.Options{})
	assertOpenAIError(t, get(t, srv.URL+"/v1/nope", ""), http.StatusNotFound, "")
}

func TestConversationIDRoundTrip(t *testing.T) {
	b := newGateBackend("hi")
	close(b.release)
	srv := newAPI(t, b, openai.Options{})
	resp := postJSON(t, srv.URL+"/v1/chat/completions", "", `{"model":"meta-think-fast","conversation_id":"conv-42","messages":[{"role":"user","content":"hi"}]}`)
	assertStatus(t, resp, http.StatusOK)
	select {
	case req := <-b.requests:
		if req.ConversationID != "conv-42" {
			t.Fatalf("ConversationID = %q, want conv-42", req.ConversationID)
		}
	case <-time.After(time.Second):
		t.Fatal("backend never received a request")
	}
}

func TestClientDisconnectCancelsBackend(t *testing.T) {
	b := newGateBackend("first")
	srv := newAPI(t, b, openai.Options{})

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"meta-think-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	if _, err := readEvent(t, br, 2*time.Second); err != nil {
		t.Fatalf("first event: %v", err)
	}
	cancel()

	// The handler's request context must be cancelled, so the backend's
	// Stream returns; the gate is never released.
	doneCh := make(chan struct{})
	go func() {
		io.Copy(io.Discard, resp.Body)
		close(doneCh)
	}()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not stop after the client disconnected")
	}
}
