// Command dgwprobe records DGW WebSocket frames from a real session.
//
// It is a recording harness, not a client: it connects with the operator's
// cookie and access token, walks the documented handshake, prints every frame
// as {"type","raw_b64","size"} and can write the frames it saw to a golden
// vector file. It cannot encode or decode frames itself — the real codec is a
// WebAssembly module that is not in this repository — so the frame type it
// prints for each payload is "unknown" and the payload is recorded verbatim.
//
// WARNING: the format of the frames it records is unverified. The tool claims no
// interoperability with the live gateway; it exists to capture bytes so a later
// codec can be checked against them.
//
// Usage:
//
//	dgwprobe -access-token TOKEN -cookie "ecto_1_sess=..." [-conversation ID]
//	         [-fbid ID] [-out dgw_golden.json] [-seconds 30]
//
// Environment fallbacks: META_ACCESS_TOKEN, META_COOKIE.
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
		frame := dgw.Frame{Direction: dgw.DirectionInbound, Type: 0, Raw: raw}
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
			Type:        "FrameType(0)",
			Description: "recorded by dgwprobe; type unknown without a codec",
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
