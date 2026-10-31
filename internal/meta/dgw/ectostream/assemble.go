package ectostream

import (
	"encoding/json"
	"fmt"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
)

// This file applies a unified-response stream to a document, the way the client
// does.
//
// The stream is not text chunks at one fixed path. A reply that uses a tool looks
// like this:
//
//	seq0  full   sections[0] = progress ("Đang tìm kiếm…")
//	seq5  full   sections[0] = progress, sections[1] = answer text
//	seq6  patch  /sections/1/view_model/primitive/text            += "Sài Gòn"
//	seq30 patch  /sections/1/view_model/primitive/inline_entities/1 = {...}
//	seq40 patch  /embedded_screens/0/content/0/tabs/1             = {...}
//
// So the answer is not at a fixed path, a "full" message replaces the whole
// document, and citations arrive in a parallel array. A decoder that reads only
// /sections/0/... answers the wrong question as soon as a progress section
// exists, and one that ignores inline_entities leaves {{IE_0}} markers in the
// text.
//
// Applying the patches is delegated to a JSON-patch implementation rather than
// hand-rolled: pointer escaping, array growth and in-place replacement are easy
// to get subtly wrong, and the failure is silent (a dropped delta, not an error).

// Assembler accumulates a unified-response stream into one document.
//
// Not safe for concurrent use; one Assembler belongs to one reply.
type Assembler struct {
	doc     map[string]any
	seq     int64
	sawFull bool
	lastErr error
}

// NewAssembler returns an empty assembler.
func NewAssembler() *Assembler {
	return &Assembler{doc: map[string]any{}}
}

// Apply feeds one raw_unified_response payload to the assembler.
func (a *Assembler) Apply(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var uf UnifiedFrame
	if err := json.Unmarshal([]byte(raw), &uf); err != nil {
		return err
	}

	switch uf.Type {
	case "full":
		if uf.Response == nil {
			return nil
		}
		b, err := json.Marshal(uf.Response)
		if err != nil {
			return err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		a.doc = m
		a.sawFull = true
		a.seq = uf.Seq
	case "patch":
		for _, op := range uf.Operations {
			if err := a.applyOp(op); err != nil {
				// Remember the first failure but keep applying: one bad
				// operation must not discard the rest of the answer.
				if a.lastErr == nil {
					a.lastErr = err
				}
			}
		}
		a.seq = uf.Seq
	}
	return nil
}

// SawFull reports whether a full document was received.
func (a *Assembler) SawFull() bool { return a.sawFull }

// Err returns the first operation that could not be applied, if any.
func (a *Assembler) Err() error { return a.lastErr }

// Document returns the accumulated document.
func (a *Assembler) Document() map[string]any { return a.doc }

// applyOp applies one operation.
//
// "delta" is meta.ai's own op: append a string at a path. It is translated to a
// read-then-replace, because JSON patch has no append. Everything else is
// standard JSON patch and goes through the library.
func (a *Assembler) applyOp(op UnifiedOperation) error {
	if op.Op != "delta" {
		patch, err := json.Marshal([]map[string]any{{
			"op":    op.Op,
			"path":  op.Path,
			"value": json.RawMessage(op.Value),
		}})
		if err != nil {
			return err
		}
		decoded, err := jsonpatch.DecodePatch(patch)
		if err != nil {
			return fmt.Errorf("ectostream: %s %s: %w", op.Op, op.Path, err)
		}
		cur, err := json.Marshal(a.doc)
		if err != nil {
			return err
		}
		out, err := decoded.Apply(cur)
		if err != nil {
			return fmt.Errorf("ectostream: %s %s: %w", op.Op, op.Path, err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			return err
		}
		a.doc = m
		return nil
	}

	// delta: append.
	var val any
	if len(op.Value) > 0 {
		if err := json.Unmarshal(op.Value, &val); err != nil {
			return err
		}
	}
	s, isStr := val.(string)

	cur, err := valueAt(a.doc, op.Path)
	if err != nil {
		return err
	}

	if isStr {
		existing, ok := cur.(string)
		if !ok && cur != nil {
			existing = ""
		}
		return a.replaceAt(op.Path, existing+s)
	}

	// A non-string delta extends an array.
	arr, _ := cur.([]any)
	return a.replaceAt(op.Path, append(arr, val))
}

// valueAt reads the JSON value at a pointer, returning nil when absent (which is
// the normal case for the first delta on a path).
func valueAt(doc map[string]any, pointer string) (any, error) {
	if pointer == "" {
		return doc, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("ectostream: pointer %q does not start with /", pointer)
	}
	var cur any = doc
	for _, raw := range strings.Split(pointer[1:], "/") {
		tok := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			cur = c[tok]
		case []any:
			i, err := atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil, nil
			}
			cur = c[i]
		default:
			return nil, nil
		}
	}
	return cur, nil
}

// replaceAt sets a value at a pointer, creating intermediate objects.
func (a *Assembler) replaceAt(pointer string, val any) error {
	patch, err := json.Marshal([]map[string]any{{
		"op":    "add",
		"path":  pointer,
		"value": val,
	}})
	if err != nil {
		return err
	}
	decoded, err := jsonpatch.DecodePatch(patch)
	if err != nil {
		return err
	}
	cur, err := json.Marshal(a.doc)
	if err != nil {
		return err
	}
	out, err := decoded.Apply(cur)
	if err != nil {
		return fmt.Errorf("ectostream: delta %s: %w", pointer, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return err
	}
	a.doc = m
	return nil
}

func atoi(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// Sections returns the document's sections in order.
func (a *Assembler) Sections() []map[string]any {
	raw, ok := a.doc["sections"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, s := range raw {
		if m, ok := s.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// AnswerText renders the answer, with inline-entity markers resolved.
//
// Only GenAIMarkdownTextUXPrimitive sections contribute text: progress sections
// and tool cards are structure, not prose. Including them is what made an earlier
// decoder answer the wrong question.
func (a *Assembler) AnswerText() string {
	var parts []string
	for _, sec := range a.Sections() {
		prim := primitiveOf(sec)
		if prim == nil {
			continue
		}
		if tn, _ := prim["__typename"].(string); tn != "" && tn != "GenAIMarkdownTextUXPrimitive" {
			continue
		}
		text, _ := prim["text"].(string)
		if text == "" {
			continue
		}
		parts = append(parts, resolveInlineEntities(text, prim))
	}
	return strings.Join(parts, "")
}

// ProgressText returns what the non-text sections announced, for a "searching…"
// style status line.
func (a *Assembler) ProgressText() string {
	var out []string
	for _, sec := range a.Sections() {
		prim := primitiveOf(sec)
		if prim == nil {
			continue
		}
		if tn, _ := prim["__typename"].(string); tn == "GenAIMarkdownTextUXPrimitive" {
			continue
		}
		if t, ok := prim["text"].(string); ok && t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}

// Media returns every generated image or video in the reply, in section order.
//
// A generated image arrives as a GenAIImagePrimitive section:
//
//	{"__typename":"GenAIImagePrimitive",
//	 "preview_image":{"url":"https://scontent...webp","mime_type":"image/webp",
//	                  "width":1920,"height":1280},
//	 "full_image":{...same shape...},
//	 "source":{"source_type":"IMAGE_GEN","source_display_name":"gallery/image_....webp"},
//	 "image_type":"DEFAULT"}
//
// The url is a signed CDN link, so it carries its own expiry and must not be
// cached indefinitely. This is verified against a live generation.
func (a *Assembler) Media() []MediaItem {
	var out []MediaItem
	for _, sec := range a.Sections() {
		prim := primitiveOf(sec)
		if prim == nil {
			continue
		}
		tn, _ := prim["__typename"].(string)
		if !strings.Contains(tn, "Image") && !strings.Contains(tn, "Video") {
			continue
		}
		if m, ok := mediaFrom(prim); ok {
			out = append(out, m)
		}
	}
	return out
}

// MediaItem is one generated image or video.
type MediaItem struct {
	// URL is the full-size asset.
	URL string
	// PreviewURL is the smaller rendition, when the server sent a different one.
	PreviewURL string
	// MimeType, Width and Height describe the asset.
	MimeType string
	Width    int
	Height   int
	// Source names where the asset sits in the user's gallery.
	Source string
	// MediaID is the server-side identifier, when present.
	MediaID string
}

func mediaFrom(prim map[string]any) (MediaItem, bool) {
	full, _ := prim["full_image"].(map[string]any)
	if full == nil {
		full, _ = prim["full_video"].(map[string]any)
	}
	preview, _ := prim["preview_image"].(map[string]any)
	if preview == nil {
		preview, _ = prim["preview_video"].(map[string]any)
	}
	if full == nil && preview == nil {
		return MediaItem{}, false
	}

	src, _ := prim["source"].(map[string]any)
	item := MediaItem{
		Source: str(src["source_display_name"]),
	}
	if full != nil {
		item.URL = str(full["url"])
		item.MimeType = str(full["mime_type"])
		item.MediaID = str(full["media_id"])
		item.Width = intOf(full["width"])
		item.Height = intOf(full["height"])
	}
	if preview != nil {
		item.PreviewURL = str(preview["url"])
		if item.URL == "" {
			item.URL = item.PreviewURL
		}
		if item.MimeType == "" {
			item.MimeType = str(preview["mime_type"])
		}
		if item.MediaID == "" {
			item.MediaID = str(preview["media_id"])
		}
		if item.Width == 0 {
			item.Width = intOf(preview["width"])
			item.Height = intOf(preview["height"])
		}
	}
	return item, item.URL != ""
}

// intOf reads a JSON number that may have arrived as float64 or json.Number.
func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// Citations returns every inline entity carrying a url, in section order.
func (a *Assembler) Citations() []Citation {
	var out []Citation
	seen := map[string]bool{}
	for _, sec := range a.Sections() {
		prim := primitiveOf(sec)
		if prim == nil {
			continue
		}
		ents, ok := prim["inline_entities"].([]any)
		if !ok {
			continue
		}
		for _, e := range ents {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			md, _ := m["metadata"].(map[string]any)
			if md == nil {
				md = m
			}
			c := Citation{
				Title: firstNonEmpty(str(md["reference_title"]), str(md["title"])),
				URL:   firstNonEmpty(str(md["reference_url"]), str(md["url"])),
				Site:  firstNonEmpty(str(md["reference_display_name"]), str(md["source_display_name"])),
			}
			if c.URL == "" || seen[c.URL] {
				continue
			}
			seen[c.URL] = true
			out = append(out, c)
		}
	}
	return out
}

// Citation is one resolved source.
type Citation struct {
	Title string
	URL   string
	Site  string
}

func primitiveOf(sec map[string]any) map[string]any {
	vm, ok := sec["view_model"].(map[string]any)
	if !ok {
		return nil
	}
	p, _ := vm["primitive"].(map[string]any)
	return p
}

// resolveInlineEntities replaces {{IE_<n>}}anchor{{/IE_<n>}} with a readable
// citation marker, and never leaves a raw marker in the output.
//
// The entity shape, from a recorded reply:
//
//	{"key":"IE_0",
//	 "metadata":{"__typename":"GenAISearchCitationItem",
//	             "reference_url":"https://laodong.vn/...",
//	             "reference_title":"Dự báo thời tiết ngày 31.10 tại TPHCM và Nam Bộ",
//	             "reference_display_name":"laodong.vn"}}
//
// and the marker body is a short id ("600b"), not the title. Printing the body
// leaves an opaque hash in the answer, which is what the first implementation
// did. The visible text becomes "[laodong.vn]" and the anchor text is dropped,
// because the same source can be cited several times and repeating the body
// adds nothing a reader can use.
func resolveInlineEntities(text string, prim map[string]any) string {
	ents, _ := prim["inline_entities"].([]any)

	// The marker names the entity twice over: "{{IE_0}}" carries the digit, and
	// the entity's own key is "IE_0". Index by both, and by position, because
	// which of the three the server uses has varied in recorded traffic.
	byKey := map[string]map[string]any{}
	for i, e := range ents {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if k, ok := m["key"].(string); ok {
			byKey[k] = m
			byKey[strings.TrimPrefix(k, "IE_")] = m
		}
		byKey[itoa(i)] = m
		byKey[itoa(i+1)] = m
	}

	var b strings.Builder
	rest := text
	for {
		start := strings.Index(rest, "{{IE_")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		// Index within the suffix, then make it absolute. Mixing the two gives a
		// truncated label ("0" instead of "IE_0"), which silently matches no
		// entity and drops the citation.
		relEnd := strings.Index(rest[start:], "}}")
		if relEnd < 0 {
			rest = rest[start+2:]
			continue
		}
		// Absolute index of "}}"; the label sits between "{{IE_" and it.
		closeAt := start + relEnd
		label := rest[start+len("{{IE_") : closeAt]
		bodyStart := closeAt + 2
		closer := "{{/IE_" + label + "}}"
		bodyEnd := strings.Index(rest[bodyStart:], closer)
		if bodyEnd < 0 {
			b.WriteString(rest[bodyStart:])
			rest = ""
			break
		}
		rest = rest[bodyStart+bodyEnd+len(closer):]

		if e, ok := byKey[label]; ok {
			if label := citationLabel(e); label != "" {
				b.WriteString(label)
				continue
			}
		}
		// No usable entity: drop the marker and its opaque body rather than
		// leaving a hash in the prose.
	}
	return b.String()
}

// citationLabel renders one entity as something a reader can use.
func citationLabel(e map[string]any) string {
	md, _ := e["metadata"].(map[string]any)
	if md == nil {
		// Some shapes put the fields at the top level.
		md = e
	}
	name := firstNonEmpty(
		str(md["reference_display_name"]),
		str(md["source_display_name"]),
		str(md["reference_title"]),
		str(md["title"]),
	)
	if name == "" {
		return ""
	}
	return "[" + name + "]"
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
