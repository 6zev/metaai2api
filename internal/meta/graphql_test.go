package meta

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/proxy"
)

const testCookie = "ecto_1_sess=s%3Asess-value.test-signature; datr=datr-value"

// newClient points c at srv and installs the server's transport so every
// request traverses the test handler. It returns the request recorder, or nil:
// requests to a stubbed endpoint may never reach the handler if the response is
// written entirely before the body is read.
func newClient(t *testing.T, srv *httptest.Server, cookie string) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv.Config.Handler = rec.handler(srv.Config.Handler)
	tr, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httptest client transport is %T, want *http.Transport", srv.Client().Transport)
	}
	proxy.Provide(tr)
	t.Cleanup(func() { proxy.Provide(nil) })

	c, err := New(Options{Endpoint: srv.URL + "/api/graphql", Cookie: cookie})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, rec
}

// recorder captures the last request's method, URL and headers so tests can
// assert on them. It does not touch the body: the handler owns that.
type recorder struct {
	last *http.Request
}

func (r *recorder) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.last = req.Clone(req.Context())
		next.ServeHTTP(w, req)
	})
}

func TestGraphQL(t *testing.T) {
	t.Run("posts relay body and browser headers", func(t *testing.T) {
		var body map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if got := r.URL.Path; got != "/api/graphql" {
				t.Errorf("path = %q, want /api/graphql", got)
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"node":{"id":"abc"}}}`))
		}))
		defer srv.Close()

		c, rec := newClient(t, srv, testCookie)
		c.lsd = "test-lsd"
		raw, err := c.GraphQLNamed(t.Context(), NameDocEctoHomePromptRouteQuery, DocEctoHomePromptRouteQuery, map[string]any{"id": "abc"})
		if err != nil {
			t.Fatalf("GraphQL: %v", err)
		}
		if got := string(raw); got != `{"data":{"node":{"id":"abc"}}}` {
			t.Errorf("raw = %s", got)
		}

		if got := rec.last.Header.Get("Accept"); got != Accept {
			t.Errorf("accept = %q, want %q", got, Accept)
		}
		if got := rec.last.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		for header, want := range map[string]string{
			"Origin":         "https://meta.ai",
			"Referer":        "https://meta.ai/",
			"User-Agent":     userAgent,
			"X-Fb-Friendly":  "EctoHomePromptRouteQuery",
			"X-Fb-Lsd":       "test-lsd",
			"Sec-Fetch-Site": "same-origin",
			"Sec-Fetch-Mode": "cors",
			"Sec-Fetch-Dest": "empty",
		} {
			if got := rec.last.Header.Get(header); got != want {
				t.Errorf("%s = %q, want %q", header, got, want)
			}
		}
		if got := rec.last.Header.Get("Cookie"); got != testCookie {
			t.Errorf("cookie = %q, want %q", got, testCookie)
		}

		docID, _ := body["doc_id"].(string)
		if docID != DocEctoHomePromptRouteQuery {
			t.Errorf("doc_id = %q", docID)
		}
		vars, _ := body["variables"].(map[string]any)
		if vars["id"] != "abc" {
			t.Errorf("variables = %v", body["variables"])
		}
	})

	t.Run("extracts multipart part", func(t *testing.T) {
		fixture, err := os.ReadFile(filepath.Join("testdata", "multipart.bin"))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", `multipart/mixed; boundary="-"; deferSpec=20220824`)
			_, _ = w.Write(fixture)
		}))
		defer srv.Close()

		c, _ := newClient(t, srv, testCookie)
		raw, err := c.GraphQL(t.Context(), DocEctoMergedMessageListPreloadQuery, nil)
		if err != nil {
			t.Fatalf("GraphQL: %v", err)
		}
		if !json.Valid(raw) {
			t.Fatalf("result is not valid JSON: %s", raw)
		}
		var got struct {
			Data struct {
				Conversation struct {
					ID string `json:"id"`
				} `json:"conversation"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("parse result: %v", err)
		}
		if got.Data.Conversation.ID != "c1f0b6a2-9e59-4cc9-9b3f-0d6f7b1f6a2e" {
			t.Errorf("conversation id = %q", got.Data.Conversation.ID)
		}
	})

	t.Run("maps graphql errors to a protocol error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"errors":[{"message":"Cannot query field \"nope\"","severity":"ERROR"}]}`))
		}))
		defer srv.Close()

		c, _ := newClient(t, srv, testCookie)
		_, err := c.GraphQL(t.Context(), DocEctoHomePromptRouteQuery, nil)
		if !errors.Is(err, backend.ErrUpstreamProtocol) {
			t.Fatalf("err = %v, want ErrUpstreamProtocol", err)
		}
		if !strings.Contains(err.Error(), "Cannot query field") {
			t.Errorf("err = %v, want upstream message included", err)
		}
		if errors.Is(err, backend.ErrSessionExpired) {
			t.Errorf("err = %v, must not be ErrSessionExpired", err)
		}
	})

	t.Run("maps auth-shaped graphql errors to session expired", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"errors":[{"message":"User not logged in","severity":"CRITICAL"}]}`))
		}))
		defer srv.Close()

		c, _ := newClient(t, srv, testCookie)
		_, err := c.GraphQL(t.Context(), DocEctoHomePromptRouteQuery, nil)
		if !errors.Is(err, backend.ErrSessionExpired) {
			t.Fatalf("err = %v, want ErrSessionExpired", err)
		}
	})
}

func TestGraphQLSessionExpired(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			c, _ := newClient(t, srv, testCookie)
			_, err := c.GraphQL(t.Context(), DocEctoHomePromptRouteQuery, nil)
			if !errors.Is(err, backend.ErrSessionExpired) {
				t.Fatalf("err = %v, want ErrSessionExpired", err)
			}
		})
	}
}

func TestGraphQLRequiresCookie(t *testing.T) {
	if _, err := New(Options{}); !errors.Is(err, backend.ErrSessionExpired) {
		t.Fatalf("err = %v, want ErrSessionExpired", err)
	}
}

func TestMultipart(t *testing.T) {
	// parseTestBody builds a body with CRLF delimiters, the way GraphQL serves
	// multipart/mixed. A bare-LF fixture would not survive mime/multipart.
	parseTestBody := func(t *testing.T, contentType string, body string) []json.RawMessage {
		t.Helper()
		parts, err := parseMultipart(contentType, strings.NewReader(body))
		if err != nil {
			t.Fatalf("parseMultipart: %v", err)
		}
		return parts
	}

	t.Run("fixture decodes every part and last has hasNext false", func(t *testing.T) {
		fixture, err := os.ReadFile(filepath.Join("testdata", "multipart.bin"))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		ct := `multipart/mixed; boundary="-"; deferSpec=20220824`
		parts, err := parseMultipart(ct, strings.NewReader(string(fixture)))
		if err != nil {
			t.Fatalf("parseMultipart: %v", err)
		}
		if len(parts) < 2 {
			t.Fatalf("got %d parts, want at least 2", len(parts))
		}
		for i, p := range parts {
			if !json.Valid(p) {
				t.Errorf("part %d is not JSON: %s", i, p)
			}
		}
		first := string(parts[0])
		if !strings.Contains(first, `"data"`) {
			t.Errorf("part 0 = %s, want initial data part", first)
		}
		last := parts[len(parts)-1]
		var envelope struct {
			HasNext bool            `json:"hasNext"`
			Path    []string        `json:"path"`
			Label   string          `json:"label"`
			Data    json.RawMessage `json:"data"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(last, &envelope); err != nil {
			t.Fatalf("parse last part: %v", err)
		}
		if envelope.HasNext {
			t.Errorf("last part hasNext = true, want false")
		}
		if len(envelope.Data) == 0 && len(envelope.Errors) == 0 {
			t.Errorf("last part carries neither data nor errors: %s", last)
		}

		// The fixture patch carries label/path, so it lands at
		// data.conversation.messages while the first part's fields survive.
		merged, err := mergeParts(parts)
		if err != nil {
			t.Fatalf("mergeParts: %v", err)
		}
		var got struct {
			Data struct {
				Conversation struct {
					ID       string `json:"id"`
					Title    string `json:"title"`
					Messages struct {
						Edges []struct {
							Node struct {
								ID string `json:"id"`
							} `json:"node"`
						} `json:"edges"`
					} `json:"messages"`
				} `json:"conversation"`
			} `json:"data"`
		}
		if err := json.Unmarshal(merged, &got); err != nil {
			t.Fatalf("parse merged: %v", err)
		}
		conv := got.Data.Conversation
		if conv.ID != "c1f0b6a2-9e59-4cc9-9b3f-0d6f7b1f6a2e" {
			t.Errorf("id = %q", conv.ID)
		}
		if conv.Title != "Shortest distance between two points in a city" {
			t.Errorf("title = %q", conv.Title)
		}
		if len(conv.Messages.Edges) != 1 || conv.Messages.Edges[0].Node.ID != "m-0001" {
			t.Errorf("messages = %+v", conv.Messages)
		}
	})

	t.Run("extra parts are merged, not dropped", func(t *testing.T) {
		body := "--graphql\r\n" +
			"Content-Type: application/json; charset=utf-8\r\n\r\n" +
			"{\"data\":{\"a\":{\"x\":1},\"b\":2}}\r\n" +
			"--graphql\r\n" +
			"Content-Type: application/json; charset=utf-8\r\n\r\n" +
			"{\"path\":[\"a\"],\"data\":{\"y\":3},\"label\":\"L1\",\"hasNext\":true}\r\n" +
			"--graphql\r\n" +
			"Content-Type: application/json; charset=utf-8\r\n\r\n" +
			"{\"path\":[\"a\"],\"data\":{\"z\":4},\"label\":\"L1\",\"hasNext\":false}\r\n" +
			"--graphql--\r\n"
		parts := parseTestBody(t, "multipart/mixed; boundary=graphql", body)
		if len(parts) != 3 {
			t.Fatalf("got %d parts, want 3", len(parts))
		}
		merged, err := mergeParts(parts)
		if err != nil {
			t.Fatalf("mergeParts: %v", err)
		}
		var got struct {
			Data struct {
				A map[string]int `json:"a"`
				B int            `json:"b"`
			} `json:"data"`
		}
		if err := json.Unmarshal(merged, &got); err != nil {
			t.Fatalf("parse merged: %v", err)
		}
		for want, v := range map[string]int{"x": 1, "y": 3, "z": 4} {
			if got.Data.A[want] != v {
				t.Errorf("data.a[%q] = %d, want %d (merged = %s)", want, got.Data.A[want], v, merged)
			}
		}
		if got.Data.B != 2 {
			t.Errorf("data.b = %d, want 2", got.Data.B)
		}
	})

	t.Run("error part fails instead of being dropped", func(t *testing.T) {
		body := "--b\r\nContent-Type: application/json\r\n\r\n" +
			"{\"data\":null,\"errors\":[{\"message\":\"boom\"}],\"hasNext\":false}\r\n--b--\r\n"
		parts := parseTestBody(t, "multipart/mixed; boundary=b", body)
		_, err := mergeParts(parts)
		if !errors.Is(err, backend.ErrUpstreamProtocol) {
			t.Fatalf("err = %v, want ErrUpstreamProtocol", err)
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("err = %v, want upstream message", err)
		}
	})

	t.Run("malformed part is an error", func(t *testing.T) {
		body := "--b\r\nContent-Type: application/json\r\n\r\nnot json\r\n--b--\r\n"
		_, err := parseMultipart("multipart/mixed; boundary=b", strings.NewReader(body))
		if !errors.Is(err, backend.ErrUpstreamProtocol) {
			t.Fatalf("err = %v, want ErrUpstreamProtocol", err)
		}
	})

	t.Run("stream ending with hasNext true is an error", func(t *testing.T) {
		body := "--b\r\nContent-Type: application/json\r\n\r\n" +
			"{\"data\":{},\"hasNext\":true}\r\n--b--\r\n"
		parts := parseTestBody(t, "multipart/mixed; boundary=b", body)
		if _, err := mergeParts(parts); !errors.Is(err, backend.ErrUpstreamProtocol) {
			t.Fatalf("err = %v, want ErrUpstreamProtocol", err)
		}
	})

	t.Run("missing boundary is an error", func(t *testing.T) {
		_, err := parseMultipart("multipart/mixed", strings.NewReader("--\r\n"))
		if !errors.Is(err, backend.ErrUpstreamProtocol) {
			t.Fatalf("err = %v, want ErrUpstreamProtocol", err)
		}
	})

	t.Run("empty body is an error", func(t *testing.T) {
		_, err := parseMultipart("multipart/mixed; boundary=b", strings.NewReader("--b--\r\n"))
		if !errors.Is(err, backend.ErrUpstreamProtocol) {
			t.Fatalf("err = %v, want ErrUpstreamProtocol", err)
		}
	})
}
