package ectostream_test

import (
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/meta/dgw/ectostream"
)

// The reply shapes below are taken from a recorded weather answer: a progress
// section, the answer at sections[1] (not sections[0]), an inline citation, and
// an embedded tool screen.

const fullProgress = `{"seq":0,"type":"full","response":{"response_id":"r1",
 "sections":[{"view_model":{"__typename":"GenAISingleLayoutViewModel",
  "primitive":{"__typename":"GenAIProgressUXPrimitive","text":"Đang tìm kiếm…"}}}]}}`

const fullAnswer = `{"seq":5,"type":"full","response":{"response_id":"r1",
 "sections":[
  {"view_model":{"__typename":"GenAISingleLayoutViewModel",
   "primitive":{"__typename":"GenAIProgressUXPrimitive","text":"Đang tìm kiếm…"}}},
  {"view_model":{"__typename":"GenAISingleLayoutViewModel",
   "primitive":{"__typename":"GenAIMarkdownTextUXPrimitive","text":"Sài Gòn"}}}]}}`

const deltaText = `{"seq":6,"type":"patch","operations":[
 {"op":"delta","path":"/sections/1/view_model/primitive/text","value":" hôm nay mưa"}]}`

const deltaCitation = `{"seq":7,"type":"patch","operations":[
 {"op":"add","path":"/sections/1/view_model/primitive/inline_entities",
  "value":[{"key":"IE_0","metadata":{"__typename":"GenAISearchCitationItem",
   "reference_url":"https://laodong.vn/a","reference_title":"Dự báo thời tiết",
   "reference_display_name":"laodong.vn"}}]},
 {"op":"delta","path":"/sections/1/view_model/primitive/text",
  "value":" {{IE_0}}600b{{/IE_0}}"}]}`

// TestAssemblerAnswersTheRightSection pins the bug that mattered: the answer is
// at sections[1] when a progress section exists, so a fixed /sections/0 read
// returns the progress line instead.
func TestAssemblerAnswersTheRightSection(t *testing.T) {
	a := ectostream.NewAssembler()
	for _, m := range []string{fullProgress, fullAnswer} {
		if err := a.Apply(m); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	got := a.AnswerText()
	if !strings.Contains(got, "Sài Gòn") {
		t.Fatalf("answer text missing: %q", got)
	}
	if strings.Contains(got, "Đang tìm kiếm") {
		t.Fatalf("progress text leaked into the answer: %q", got)
	}
	if p := a.ProgressText(); !strings.Contains(p, "Đang tìm kiếm") {
		t.Fatalf("progress text not reported separately: %q", p)
	}
}

// TestAssemblerAppendsDeltas verifies "delta" appends rather than replaces.
func TestAssemblerAppendsDeltas(t *testing.T) {
	a := ectostream.NewAssembler()
	for _, m := range []string{fullAnswer, deltaText} {
		if err := a.Apply(m); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if got := a.AnswerText(); !strings.Contains(got, "Sài Gòn hôm nay mưa") {
		t.Fatalf("delta not appended: %q", got)
	}
}

// TestAssemblerResolvesCitations checks both halves: the marker becomes readable
// text and the source is listed.
func TestAssemblerResolvesCitations(t *testing.T) {
	a := ectostream.NewAssembler()
	for _, m := range []string{fullAnswer, deltaCitation} {
		if err := a.Apply(m); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	got := a.AnswerText()
	if strings.Contains(got, "{{IE_") {
		t.Fatalf("marker left in the answer: %q", got)
	}
	// The opaque body id must not survive; it is what the first version printed.
	if strings.Contains(got, "600b") {
		t.Fatalf("anchor id printed instead of a label: %q", got)
	}
	if !strings.Contains(got, "[laodong.vn]") {
		t.Fatalf("citation label missing: %q", got)
	}

	cites := a.Citations()
	if len(cites) != 1 {
		t.Fatalf("citations = %d, want 1: %+v", len(cites), cites)
	}
	if cites[0].URL != "https://laodong.vn/a" || cites[0].Site != "laodong.vn" {
		t.Fatalf("citation = %+v", cites[0])
	}
}

// TestAssemblerFullReplacesDocument pins that a later full message replaces the
// document rather than merging into it: the server sends one at the end carrying
// the complete answer.
func TestAssemblerFullReplacesDocument(t *testing.T) {
	a := ectostream.NewAssembler()
	for _, m := range []string{fullAnswer, deltaText} {
		if err := a.Apply(m); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}
	if got := a.AnswerText(); strings.Count(got, "Sài Gòn") != 1 {
		t.Fatalf("precondition: %q", got)
	}

	// A fresh full message replaces everything.
	final := `{"seq":99,"type":"full","response":{"response_id":"r1",
	 "sections":[{"view_model":{"__typename":"GenAISingleLayoutViewModel",
	  "primitive":{"__typename":"GenAIMarkdownTextUXPrimitive","text":"Câu trả lời cuối"}}}]}}`
	if err := a.Apply(final); err != nil {
		t.Fatalf("Apply final: %v", err)
	}
	if got := a.AnswerText(); got != "Câu trả lời cuối" {
		t.Fatalf("full did not replace: %q", got)
	}
}

// TestAssemblerBadOperationKeepsAnswer keeps one malformed operation from
// discarding a reply that is otherwise fine.
func TestAssemblerBadOperationKeepsAnswer(t *testing.T) {
	a := ectostream.NewAssembler()
	if err := a.Apply(fullAnswer); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	bad := `{"seq":9,"type":"patch","operations":[
	 {"op":"remove","path":"/does/not/exist/at/all"},
	 {"op":"delta","path":"/sections/1/view_model/primitive/text","value":" ok"}]}`
	if err := a.Apply(bad); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := a.AnswerText(); !strings.Contains(got, "ok") {
		t.Fatalf("one bad op discarded a good one: %q", got)
	}
	if a.Err() == nil {
		t.Fatal("the failed operation should be reported")
	}
}

// The image shape below is from a live generation: a GenAIImagePrimitive
// section carrying a signed CDN url, which is what a caller needs to show or
// download the picture.
const fullImage = `{"seq":80,"type":"full","response":{"response_id":"r1",
 "sections":[
  {"view_model":{"__typename":"GenAISingleLayoutViewModel",
   "primitive":{"__typename":"GenAIMarkdownTextUXPrimitive","text":"Đây nè:"}}},
  {"view_model":{"__typename":"GenAISingleLayoutViewModel",
   "primitive":{"__typename":"GenAIImagePrimitive",
    "preview_image":{"url":"https://cdn/preview.webp","mime_type":"image/webp",
                     "width":1920,"height":1280},
    "full_image":{"url":"https://cdn/full.webp","mime_type":"image/webp",
                  "width":1920,"height":1280},
    "source":{"source_type":"IMAGE_GEN",
              "source_display_name":"gallery/image_20261031_144644.webp"},
    "image_type":"DEFAULT"}}}]}}`

// TestAssemblerExtractsGeneratedImage covers the case that matters for image
// generation: the asset is a url inside a GenAIImagePrimitive section, not text.
func TestAssemblerExtractsGeneratedImage(t *testing.T) {
	a := ectostream.NewAssembler()
	if err := a.Apply(fullImage); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	media := a.Media()
	if len(media) != 1 {
		t.Fatalf("media = %d, want 1: %+v", len(media), media)
	}
	m := media[0]
	if m.URL != "https://cdn/full.webp" {
		t.Errorf("url = %q, want the full-size asset", m.URL)
	}
	if m.PreviewURL != "https://cdn/preview.webp" {
		t.Errorf("preview = %q", m.PreviewURL)
	}
	if m.Width != 1920 || m.Height != 1280 {
		t.Errorf("dimensions = %dx%d", m.Width, m.Height)
	}
	if m.MimeType != "image/webp" {
		t.Errorf("mime = %q", m.MimeType)
	}
	if m.Source != "gallery/image_20261031_144644.webp" {
		t.Errorf("source = %q", m.Source)
	}

	// The image section must not leak into the prose.
	if got := a.AnswerText(); strings.Contains(got, "cdn/") {
		t.Fatalf("image url leaked into the answer text: %q", got)
	}
}

// TestAssemblerNoMediaForPlainText keeps a text reply from reporting phantom
// media.
func TestAssemblerNoMediaForPlainText(t *testing.T) {
	a := ectostream.NewAssembler()
	if err := a.Apply(fullAnswer); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := a.Media(); len(got) != 0 {
		t.Fatalf("text reply reported %d media items", len(got))
	}
}

// The video shape is different from the image's: one "video" key rather than
// full_image/preview_image, plus a duration.
const fullVideo = `{"seq":90,"type":"full","response":{"response_id":"r1",
 "sections":[
  {"view_model":{"__typename":"GenAISingleLayoutViewModel",
   "primitive":{"__typename":"GenAIMarkdownTextUXPrimitive","text":"Here's your video:"}}},
  {"view_model":{"__typename":"GenAISingleLayoutViewModel",
   "primitive":{"__typename":"GenAIVideoPrimitive",
    "filename":"image_20261031_145318.mp4",
    "video":{"url":"https://cdn/clip.mp4","mime_type":"video/mp4",
             "width":1280,"height":720,"duration":10000},
    "video_duration_ms":10000}}}]}}`

func TestAssemblerExtractsGeneratedVideo(t *testing.T) {
	a := ectostream.NewAssembler()
	if err := a.Apply(fullVideo); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	media := a.Media()
	if len(media) != 1 {
		t.Fatalf("media = %d, want 1: %+v", len(media), media)
	}
	m := media[0]
	if m.URL != "https://cdn/clip.mp4" {
		t.Errorf("url = %q", m.URL)
	}
	if m.MimeType != "video/mp4" {
		t.Errorf("mime = %q", m.MimeType)
	}
	if m.DurationMs != 10000 {
		t.Errorf("duration = %d, want 10000", m.DurationMs)
	}
	if m.Source != "image_20261031_145318.mp4" {
		t.Errorf("source = %q", m.Source)
	}
	if m.Width != 1280 || m.Height != 720 {
		t.Errorf("dims = %dx%d", m.Width, m.Height)
	}
}
