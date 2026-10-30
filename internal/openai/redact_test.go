package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/openai"
)

// echoBackend fails with an error whose text quotes whatever it is given. It
// models the real risk: meta.ai's GraphQL client embeds part of the upstream
// response body in its error, so a cookie echoed by the far side would reach
// the caller.
type echoBackend struct {
	err error
}

func (b *echoBackend) Name() string { return "echo" }

func (b *echoBackend) Complete(context.Context, backend.ChatRequest) (backend.ChatResult, error) {
	return backend.ChatResult{}, b.err
}

func (b *echoBackend) Stream(ctx context.Context, _ backend.ChatRequest, emit func(backend.Delta) error) error {
	if err := emit(backend.Delta{Text: "partial"}); err != nil {
		return err
	}
	return b.err
}

const (
	echoCookie    = "datr=abc; ecto_1_sess=SECRET-SESSION-VALUE-8471"
	echoAccess    = "ACCESS-TOKEN-VALUE-9922"
	echoAPI       = "API-TOKEN-VALUE-3311"
	echoAdmin     = "ADMIN-TOKEN-VALUE-7704"
	allEchoedText = echoCookie + " " + echoAccess + " " + echoAPI + " " + echoAdmin
)

func TestV1ErrorsDoNotLeakSecrets(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		b := &echoBackend{err: errors.Join(backend.ErrUpstreamProtocol,
			errors.New("upstream said: "+allEchoedText))}
		srv := newAPI(t, b, openai.Options{Secrets: []string{echoCookie, echoAccess, echoAPI, echoAdmin}})
		defer srv.Close()

		resp := postChat(t, srv.URL, `{"model":"meta-think-fast","messages":[{"role":"user","content":"hi"}]}`)
		body, _ := io.ReadAll(resp.Body)
		assertNoSecretsIn(t, string(body))
	})

	t.Run("stream", func(t *testing.T) {
		b := &echoBackend{err: errors.Join(backend.ErrUpstreamProtocol,
			errors.New("stream died: "+allEchoedText))}
		srv := newAPI(t, b, openai.Options{Secrets: []string{echoCookie, echoAccess, echoAPI, echoAdmin}})
		defer srv.Close()

		resp := postChat(t, srv.URL, `{"model":"meta-think-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		body, _ := io.ReadAll(resp.Body)
		assertNoSecretsIn(t, string(body))
		// The SSE path must still terminate cleanly after the in-band error.
		if !strings.Contains(string(body), "data: [DONE]") {
			t.Fatalf("stream did not terminate with [DONE]:\n%s", body)
		}
	})

	t.Run("api token is scrubbed without being listed", func(t *testing.T) {
		// An operator configuring api_token should not have to remember to add
		// it to Secrets; it is added automatically.
		b := &echoBackend{err: errors.New("rejected: " + echoAPI)}
		srv := newAPI(t, b, openai.Options{APIToken: echoAPI})
		defer srv.Close()

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"meta-think-fast","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+echoAPI)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		assertNoSecretsIn(t, string(body))
	})
}

func postChat(t *testing.T, base, payload string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func assertNoSecretsIn(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{
		"SECRET-SESSION-VALUE-8471",
		"ACCESS-TOKEN-VALUE-9922",
		"API-TOKEN-VALUE-3311",
		"ADMIN-TOKEN-VALUE-7704",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaks %q:\n%s", secret, body)
		}
	}
}

// TestRedactPackageIsUsed pins that the shared helper actually masks, so a
// future refactor cannot quietly turn scrubbing into a no-op.
func TestSecretsAreActuallyRedactedInErrorShape(t *testing.T) {
	b := &echoBackend{err: errors.New("leak " + echoAPI)}
	srv := newAPI(t, b, openai.Options{APIToken: echoAPI})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"meta-think-fast","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+echoAPI)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var decoded struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(decoded.Error.Message, "leak") {
		t.Fatalf("error message lost its meaning entirely: %q", decoded.Error.Message)
	}
	if strings.Contains(decoded.Error.Message, echoAPI) {
		t.Fatalf("error message leaked the token: %q", decoded.Error.Message)
	}
}
