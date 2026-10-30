package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/meta/dgw/codec"
)

func TestRunWithoutCodecExitsNonZeroWithDiagnostic(t *testing.T) {
	if codec.BuiltIn() {
		t.Skip("this build has the codec; covered by the tagged test")
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &stdout, &stderr)

	if code == exitOK {
		t.Fatalf("run() = %d, want non-zero without the codec", code)
	}
	if code != exitDisabled {
		t.Fatalf("run() = %d, want %d (disabled)", code, exitDisabled)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing on the failure path", stdout.String())
	}
	msg := stderr.String()
	if !strings.Contains(msg, "wasm_codec") {
		t.Fatalf("stderr = %q, want it to name the wasm_codec build tag", msg)
	}
	if !strings.Contains(msg, codec.AssetName) {
		t.Fatalf("stderr = %q, want it to name %s", msg, codec.AssetName)
	}
}

func TestDescribeIsStableJSON(t *testing.T) {
	got := describe(frameFixture())
	want := `{"bytes":0,"stream_id":7,"type":"Ping"}`
	if got != want {
		t.Fatalf("describe() = %s, want %s", got, want)
	}
}
