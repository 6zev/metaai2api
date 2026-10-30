// Package meta is the upstream meta.ai client.
//
// It speaks the Relay persisted-query GraphQL API at https://meta.ai/api/graphql
// with an operator-supplied cookie. Responses are either plain JSON or a
// multipart/mixed stream of JSON parts; sequential parts of one incremental
// delivery are merged into a single JSON object, and any part carrying
// "errors" fails the call. Chat completion is not available here: meta.ai sends
// messages over the DGW WebSocket (see internal/meta/dgw).
package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/proxy"
)

const (
	// DefaultEndpoint is meta.ai's GraphQL endpoint.
	DefaultEndpoint = "https://meta.ai/api/graphql"
	// DefaultOrigin is the site origin sent with every request.
	DefaultOrigin = "https://meta.ai"

	// Accept is what the meta.ai browser sends for persisted queries.
	Accept = "multipart/mixed, application/json"

	userAgent      = "Mozilla/5.0 (X11; Linux x86_64; rv:129.0) Gecko/20100101 Firefox/129.0"
	requestTimeout = 60 * time.Second
)

// Options configures a Client.
type Options struct {
	// Cookie is the browser Cookie header value, normally jar.CookieHeader().
	// It is required: without it meta.ai answers 401 and every call fails.
	Cookie string
	// LSD is the x-fb-lsd anti-CSRF token when the operator captured one.
	LSD string
	// Endpoint overrides DefaultEndpoint; tests point it at httptest.
	Endpoint string
	// Origin overrides DefaultOrigin.
	Origin string
	// HTTPClient overrides the client; nil uses the proxy package transport.
	HTTPClient *http.Client
}

// Client is a meta.ai GraphQL client. It is safe for concurrent use.
type Client struct {
	endpoint string
	origin   string
	cookie   string
	lsd      string
	http     *http.Client
}

// New validates opts and returns a Client. A missing cookie is reported as
// backend.ErrSessionExpired: no request could authenticate without one.
func New(opts Options) (*Client, error) {
	cookie := strings.TrimSpace(opts.Cookie)
	if cookie == "" {
		return nil, fmt.Errorf("%w: meta.ai cookie is required", backend.ErrSessionExpired)
	}
	c := &Client{
		endpoint: opts.Endpoint,
		origin:   opts.Origin,
		cookie:   cookie,
		lsd:      opts.LSD,
		http:     opts.HTTPClient,
	}
	if c.endpoint == "" {
		c.endpoint = DefaultEndpoint
	}
	if c.origin == "" {
		c.origin = DefaultOrigin
	}
	return c, nil
}

// Endpoint returns the GraphQL URL this client posts to.
func (c *Client) Endpoint() string { return c.endpoint }

// GraphQL executes the persisted query docID with vars and returns the JSON of
// the response.
//
// A plain JSON response is returned as-is. A multipart/mixed response is
// consumed in full: every part must be valid JSON, sequential parts are merged,
// and a part carrying "errors" fails the call. HTTP 401/403, and in-band
// errors that report a missing or expired viewer, map to
// backend.ErrSessionExpired; other in-band errors and malformed responses map
// to backend.ErrUpstreamProtocol.
func (c *Client) GraphQL(ctx context.Context, docID string, vars any) (json.RawMessage, error) {
	if strings.TrimSpace(docID) == "" {
		return nil, fmt.Errorf("%w: empty doc_id", backend.ErrUpstreamProtocol)
	}
	return c.do(ctx, "", docID, vars)
}

// GraphQLNamed is GraphQL for a call that also names the operation, which the
// browser sends as x-fb-friendly-name. name may be empty, or one of the
// NameDoc* constants.
func (c *Client) GraphQLNamed(ctx context.Context, name, docID string, vars any) (json.RawMessage, error) {
	if strings.TrimSpace(docID) == "" {
		return nil, fmt.Errorf("%w: empty doc_id", backend.ErrUpstreamProtocol)
	}
	return c.do(ctx, name, docID, vars)
}

func (c *Client) do(ctx context.Context, name, docID string, vars any) (json.RawMessage, error) {
	payload, err := json.Marshal(map[string]any{"doc_id": docID, "variables": vars})
	if err != nil {
		return nil, fmt.Errorf("meta: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("meta: build request: %w", err)
	}
	req.Header.Set("Accept", Accept)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", c.cookie)
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Referer", c.origin+"/")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	if name != "" {
		req.Header.Set("X-Fb-Friendly", name)
	}
	if c.lsd != "" {
		req.Header.Set("X-Fb-Lsd", c.lsd)
	}

	client := c.httpClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("meta: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: meta.ai answered %s; update the cookie", backend.ErrSessionExpired, resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: meta.ai answered %s: %s", backend.ErrUpstreamProtocol, resp.Status, snippet(body))
	}

	return decodeResponse(resp.Header.Get("Content-Type"), resp.Body)
}

// httpClient builds a per-call client over the transport the proxy package
// currently publishes, so a config reload is picked up by the next request. The
// shared *http.Transport is reused; the *http.Client wrapper is cheap.
func (c *Client) httpClient() *http.Client {
	if c.http != nil {
		return c.http
	}
	return &http.Client{Transport: proxy.Current(), Timeout: requestTimeout}
}

// decodeResponse reads either plain JSON or a multipart/mixed body.
func decodeResponse(contentType string, body io.Reader) (json.RawMessage, error) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil && strings.TrimSpace(contentType) != "" {
		return nil, fmt.Errorf("%w: parse content-type %q: %w", backend.ErrUpstreamProtocol, contentType, err)
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		parts, err := parseMultipart(contentType, body)
		if err != nil {
			return nil, err
		}
		return mergeParts(parts)
	}

	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("meta: read response: %w", err)
	}
	return checkEnvelope(raw)
}

// parseMultipart splits a multipart/mixed body into the JSON payload of each
// part, in order. The boundary comes from the response content-type. Every
// part must be valid JSON; a part with any other content type, or a body with
// no parts, is a protocol error. Parts are never skipped.
func parseMultipart(contentType string, body io.Reader) ([]json.RawMessage, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("%w: parse content-type: %w", backend.ErrUpstreamProtocol, err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, fmt.Errorf("%w: content-type %q is not multipart", backend.ErrUpstreamProtocol, mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("%w: multipart response has no boundary", backend.ErrUpstreamProtocol)
	}

	// The boundary must be known before the body is buffered, so read the body
	// once and then pick the delimiter form the response actually uses.
	buffered, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("%w: read multipart body: %w", backend.ErrUpstreamProtocol, err)
	}
	// meta.ai declares boundary="-" but delimits with --"-; if that quoted form
	// is present and the declared value is not, every part is silently lost.
	bounded := resolveBoundary(buffered, boundary)
	if strings.ContainsAny(bounded, "\r\n") {
		return nil, fmt.Errorf("%w: declared boundary %q is not a header token", backend.ErrUpstreamProtocol, boundary)
	}

	mr := multipart.NewReader(bytes.NewReader(buffered), bounded)
	var parts []json.RawMessage
	for i := 0; ; i++ {
		part, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: part %d: %w", backend.ErrUpstreamProtocol, i, err)
		}
		raw, err := io.ReadAll(part)
		if err != nil {
			return nil, fmt.Errorf("%w: read part %d: %w", backend.ErrUpstreamProtocol, i, err)
		}
		raw = bytes.TrimSpace(raw)
		if !json.Valid(raw) {
			partType := part.Header.Get("Content-Type")
			return nil, fmt.Errorf("%w: part %d (%s) is not JSON: %s",
				backend.ErrUpstreamProtocol, i, partType, snippet(raw))
		}
		parts = append(parts, raw)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: multipart response had no parts", backend.ErrUpstreamProtocol)
	}
	return parts, nil
}

// resolveBoundary returns the delimiter the body uses.
//
// meta.ai sends content-type: multipart/mixed; boundary="-", which
// mime.ParseMediaType keeps literally as the three characters "-" (quotes
// included). Some clients send the unquoted "-"; mime/multipart cannot match
// that from the body, because the first line is --"- and the reader strips
// only the leading dash pair. A bare "-" is therefore promoted to the quoted
// form the body actually carries.
func resolveBoundary(body []byte, declared string) string {
	gapped := func(b string) bool {
		return bytes.Contains(body, []byte("\r\n--"+b+"\r\n")) ||
			bytes.Contains(body, []byte("\n--"+b+"\r\n"))
	}
	if gapped(declared) {
		return declared
	}
	if declared == "-" && gapped(`"-`) {
		return `"-`
	}
	return declared
}

// mergeParts combines the JSON objects of an incremental multipart response.
// The first part is the initial payload; each later part is a patch carrying a
// "path" (and usually a "label") that is applied at that path. A part with
// "errors" fails the call. hasNext on the final part is checked for shape only;
// an unexpected value is a protocol error.
func mergeParts(parts []json.RawMessage) (json.RawMessage, error) {
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: no parts to merge", backend.ErrUpstreamProtocol)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(parts[0], &top); err != nil {
		return nil, fmt.Errorf("%w: decode part 0: %w", backend.ErrUpstreamProtocol, err)
	}
	if err := checkErrors(top); err != nil {
		return nil, err
	}

	for i := 1; i < len(parts); i++ {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal(parts[i], &patch); err != nil {
			return nil, fmt.Errorf("%w: decode part %d: %w", backend.ErrUpstreamProtocol, i, err)
		}
		if err := checkErrors(patch); err != nil {
			return nil, err
		}
		if err := applyPatch(top, patch, i); err != nil {
			return nil, err
		}
	}

	if err := checkHasNext(top, len(parts)-1); err != nil {
		return nil, err
	}

	merged, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("meta: encode merged response: %w", err)
	}
	return merged, nil
}

// applyPatch writes patch's "data" at its "path" inside top. Patches without a
// path are merged into the response's "data" object (or the top level when the
// initial part had no "data").
func applyPatch(top, patch map[string]json.RawMessage, index int) error {
	data, hasData := patch["data"]
	if !hasData {
		// An incremental part must carry data; an empty patch is a protocol
		// fault rather than something to drop.
		return fmt.Errorf("%w: part %d has no data", backend.ErrUpstreamProtocol, index)
	}

	path, hasPath := patch["path"]
	if !hasPath {
		target := top
		if inner, ok := top["data"]; ok {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(inner, &obj); err != nil {
				return fmt.Errorf("%w: part %d: response data is not an object", backend.ErrUpstreamProtocol, index)
			}
			merged, err := mergeObject(obj, data, index)
			if err != nil {
				return err
			}
			top["data"] = mustMarshal(merged)
			return nil
		}
		merged, err := mergeObject(target, data, index)
		if err != nil {
			return err
		}
		top["data"] = mustMarshal(merged)
		return nil
	}

	var segments []string
	if err := json.Unmarshal(path, &segments); err != nil {
		return fmt.Errorf("%w: part %d path: %w", backend.ErrUpstreamProtocol, index, err)
	}
	if len(segments) == 0 {
		return fmt.Errorf("%w: part %d has an empty path", backend.ErrUpstreamProtocol, index)
	}

	// A GraphQL incremental patch path is rooted under "data".
	segments = append([]string{"data"}, segments...)

	// Walk from the root, rebuilding each level on the way out so the patch is
	// visible in the parent map. A missing level is created as an object.
	updated, err := setPath(top, segments, data, index, segments[1:])
	if err != nil {
		return err
	}
	for k, v := range updated {
		top[k] = v
	}
	return nil
}

func setPath(node map[string]json.RawMessage, segments []string, value json.RawMessage, index int, full []string) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(node)+1)
	for k, v := range node {
		out[k] = v
	}
	head := segments[0]
	if len(segments) == 1 {
		// Incremental data overlays the existing value at the path rather than
		// replacing it; siblings already delivered must survive.
		if raw, ok := node[head]; ok {
			var existing map[string]json.RawMessage
			if err := json.Unmarshal(raw, &existing); err == nil {
				merged, err := mergeObject(existing, value, index)
				if err != nil {
					return nil, err
				}
				out[head] = mustMarshal(merged)
				return out, nil
			}
		}
		out[head] = value
		return out, nil
	}
	var child map[string]json.RawMessage
	if raw, ok := node[head]; ok {
		if err := json.Unmarshal(raw, &child); err != nil {
			return nil, fmt.Errorf("%w: part %d path %v: %q is not an object", backend.ErrUpstreamProtocol, index, full, head)
		}
	}
	next, err := setPath(child, segments[1:], value, index, full)
	if err != nil {
		return nil, err
	}
	out[head] = mustMarshal(next)
	return out, nil
}

// mergeObject overlays the JSON object in data onto obj.
func mergeObject(obj map[string]json.RawMessage, data json.RawMessage, index int) (map[string]json.RawMessage, error) {
	var incoming map[string]json.RawMessage
	if err := json.Unmarshal(data, &incoming); err != nil {
		return nil, fmt.Errorf("%w: part %d data is not an object: %w", backend.ErrUpstreamProtocol, index, err)
	}
	out := make(map[string]json.RawMessage, len(obj)+len(incoming))
	for k, v := range obj {
		out[k] = v
	}
	for k, v := range incoming {
		out[k] = v
	}
	return out, nil
}

// checkEnvelope validates a single non-multipart JSON body.
func checkEnvelope(raw []byte) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%w: response is not JSON: %s", backend.ErrUpstreamProtocol, snippet(raw))
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("%w: response is not a JSON object: %w", backend.ErrUpstreamProtocol, err)
	}
	if err := checkErrors(top); err != nil {
		return nil, err
	}
	return raw, nil
}

// checkErrors fails on a GraphQL "errors" array. Auth-shaped messages are
// reported as ErrSessionExpired because meta.ai answers 200 with them when the
// cookie dies; everything else is ErrUpstreamProtocol. The upstream message is
// preserved in both cases.
func checkErrors(obj map[string]json.RawMessage) error {
	rawErrs, ok := obj["errors"]
	if !ok {
		return nil
	}
	var errs []struct {
		Message string `json:"message"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(rawErrs, &errs); err != nil {
		return fmt.Errorf("%w: decode errors: %w", backend.ErrUpstreamProtocol, err)
	}
	if len(errs) == 0 {
		return nil
	}

	msgs := make([]string, 0, len(errs))
	expired := false
	for _, e := range errs {
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = strings.TrimSpace(e.Summary)
		}
		msgs = append(msgs, msg)
		if isAuthError(msg) {
			expired = true
		}
	}
	joined := strings.Join(msgs, "; ")
	if expired {
		return fmt.Errorf("%w: meta.ai: %s", backend.ErrSessionExpired, joined)
	}
	return fmt.Errorf("%w: meta.ai: %s", backend.ErrUpstreamProtocol, joined)
}

func isAuthError(msg string) bool {
	m := strings.ToLower(msg)
	for _, needle := range []string{
		"not logged in",
		"logged out",
		"session expired",
		"session has expired",
		"unauthenticated",
		"unauthorized",
		"login required",
		"must be logged in",
		"checkpoint",
		"consent required",
	} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// checkHasNext rejects a multipart stream whose final part still promises more.
func checkHasNext(top map[string]json.RawMessage, last int) error {
	raw, ok := top["hasNext"]
	if !ok {
		return nil
	}
	var hasNext bool
	if err := json.Unmarshal(raw, &hasNext); err != nil {
		return fmt.Errorf("%w: part %d hasNext: %w", backend.ErrUpstreamProtocol, last, err)
	}
	if hasNext {
		return fmt.Errorf("%w: multipart stream ended with hasNext true", backend.ErrUpstreamProtocol)
	}
	return nil
}

func mustMarshal(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("meta: marshal: %v", err))
	}
	return raw
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
