// Command dgwprobe records DGW WebSocket frames from a real session.
//
// It is a recording harness, not a client: it connects with the operator's
// session, walks the documented handshake, prints every frame as
// {"type","raw_b64","size"} and can write what it saw to a golden vector file.
//
// Frame labels depend on the build:
//
//   - default build: there is no codec (the real one is dgwcppbridge.wasm, which
//     is not in this repository), so every frame is recorded verbatim and
//     labelled "unknown". Bytes are the evidence; a guessed type would not be.
//   - -tags wasm_codec with the asset present: frames are decoded and labelled
//     with their real type, and a decode failure is reported in the description.
//
// WARNING: the wire format is unverified. The tool claims no interoperability
// with the live gateway; it captures bytes so a codec can be checked against
// them.
//
// Usage:
//
//	dgwprobe [-access-token TOKEN] [-cookie VALUE] [-conversation ID] [-fbid ID]
//	         [-endpoint URL] [-proxy URL] [-out dgw_golden.json] [-seconds 30]
//
// Every flag falls back to an environment variable, and the session should be
// supplied that way: META_ACCESS_TOKEN (required) and META_COOKIE. Passing a
// token as a flag works but records it in shell history.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"net/url"

	"github.com/me/metaai2api/internal/meta/dgw"
)

// codecFor returns a frame codec when one is available in this build.
//
// Without the wasm_codec build tag there is no codec, and the prober records
// bytes it cannot interpret. That is still useful: the bytes are the evidence a
// later codec is checked against. What it must never do is label a frame with a
// type it guessed.
func codecFor() dgw.Codec {
	return availableCodec()
}

// classify describes a recorded frame. typeLabel is "unknown" when no codec is
// present, and the decode error is reported rather than hidden.
func classify(raw []byte, codec dgw.Codec) (frame dgw.Frame, typeLabel, description string) {
	frame = dgw.Frame{Direction: dgw.DirectionInbound, Raw: raw}
	if codec == nil {
		frame.Type = 0
		return frame, "unknown",
			"recorded by dgwprobe; type unknown, no frame codec in this build"
	}
	decoded, err := codec.Decode(raw)
	if err != nil {
		frame.Type = 0
		return frame, "unknown",
			"recorded by dgwprobe; codec could not decode: " + err.Error()
	}
	frame.Type = decoded.Type
	return frame, frame.Type.String(),
		"recorded by dgwprobe; decoded with the build's frame codec"
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "dgwprobe: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("dgwprobe", flag.ContinueOnError)
	var (
		accessToken  = fs.String("access-token", os.Getenv("META_ACCESS_TOKEN"), "DGW access token (Authorization query value)")
		cookie       = fs.String("cookie", os.Getenv("META_COOKIE"), "meta.ai Cookie header value")
		fbid         = fs.String("fbid", "", "viewer fb id, sent as x-dgw-uuid")
		conversation = fs.String("conversation", "", "conversation id for per-conversation grouping")
		endpoint     = fs.String("endpoint", "", "override the gateway endpoint (default "+dgw.DefaultEndpoint+")")
		proxyURL     = fs.String("proxy", "", "proxy URL for the dial (http/https/socks5/socks5h)")
		seconds      = fs.Int("seconds", 30, "how long to record before writing the golden file")
		out          = fs.String("out", "", "write a golden vector here (default: print only)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *accessToken == "" {
		return fmt.Errorf("no access token: pass -access-token or set META_ACCESS_TOKEN")
	}

	cfg := dgw.Config{
		Endpoint:       *endpoint,
		AccessToken:    *accessToken,
		Cookie:         *cookie,
		FBID:           *fbid,
		ConversationID: *conversation,
	}
	if *proxyURL != "" {
		u, err := parseProxy(*proxyURL)
		if err != nil {
			return err
		}
		cfg.ProxyURL = u
	}

	url, err := dgw.ConnectURL(cfg, nil)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "dgwprobe: dialing %s\n", redactURL(url))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := dgw.Dial(ctx, cfg, nil)
	if err != nil {
		return err
	}
	defer conn.Close()

	codec := codecFor()
	if codec == nil {
		fmt.Fprintf(os.Stderr, "dgwprobe: no frame codec in this build (rebuild with -tags wasm_codec and a dgwcppbridge.wasm to label frames)\n")
	}
	fmt.Fprintf(os.Stderr, "dgwprobe: connected; recording for %ds\n", *seconds)
	deadline := time.Now().Add(time.Duration(*seconds) * time.Second)

	var vectors []dgw.GoldenVector
	enc := json.NewEncoder(os.Stdout)
	for {
		if time.Now().After(deadline) {
			break
		}
		_ = conn.SetReadDeadline(deadline)
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		frame, typeLabel, description := classify(raw, codec)
		line, err := dgw.DescribeFrame(frame)
		if err != nil {
			return err
		}
		if err := enc.Encode(json.RawMessage(line)); err != nil {
			return err
		}
		vectors = append(vectors, dgw.GoldenVector{
			ID:          fmt.Sprintf("inbound-%04d", len(vectors)+1),
			Direction:   dgw.DirectionInbound,
			Type:        typeLabel,
			Description: description,
			RawB64:      base64.StdEncoding.EncodeToString(raw),
			DecodedJSON: []byte("null"),
		})
	}

	if *out == "" {
		return nil
	}
	set := dgw.GoldenSet{
		Version: 1,
		Source:  "dgwprobe live recording",
		Frames:  vectors,
	}
	f, err := os.Create(*out)
	if err != nil {
		return fmt.Errorf("create %s: %w", *out, err)
	}
	defer f.Close()
	je := json.NewEncoder(f)
	je.SetIndent("", "  ")
	if err := je.Encode(set); err != nil {
		return fmt.Errorf("write %s: %w", *out, err)
	}
	fmt.Fprintf(os.Stderr, "dgwprobe: wrote %d frames to %s\n", len(vectors), *out)
	return nil
}

func redactURL(raw string) string {
	// Do not print the access token: the Authorization value is a query param.
	return dgw.RedactURL(raw)
}

func parseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse proxy: %w", err)
	}
	return u, nil
}
