// Command dgwsend sends one prompt to the meta.ai DGW gateway and prints the
// decoded reply.
//
// It exists to establish, once, whether the outbound frame this repository can
// build is actually accepted, and to capture a real text reply. It is a
// diagnostic, not a service: one prompt, one conversation, then it stops.
//
// Usage:
//
//	dgwsend -token <access token> -template <recorded-prompt.bin> [-prompt "text"]
//	        [-conversation <uuid>] [-new] [-timeout 60s]
//
// -new makes a fresh conversation id, so the prompt does not land in an existing
// thread. Without it the template's own conversation id is reused, which is only
// useful for reproducing a specific bug.
package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/me/metaai2api/internal/meta/dgw"
	"github.com/me/metaai2api/internal/meta/dgw/dgwsend"
	"github.com/me/metaai2api/internal/meta/dgw/ectostream"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dgwsend:", err)
		os.Exit(1)
	}
}

func run() error {
	token := flag.String("token", os.Getenv("META_ACCESS_TOKEN"), "DGW access token (Authorization query value)")
	template := flag.String("template", "", "recorded prompt protobuf (required)")
	prompt := flag.String("prompt", "test", "prompt text to send")
	conversation := flag.String("conversation", "", "conversation id (default: the template's)")
	newConv := flag.Bool("new", false, "generate a fresh conversation id")
	timeout := flag.Duration("timeout", 60*time.Second, "how long to wait for the reply")
	raw := flag.Bool("raw", false, "print every received frame as hex instead of decoding")
	flag.Parse()

	if *token == "" {
		return fmt.Errorf("no token: pass -token or set META_ACCESS_TOKEN")
	}
	if *template == "" {
		return fmt.Errorf("no template: pass -template <recorded-prompt.bin>")
	}

	tplRaw, err := os.ReadFile(*template)
	if err != nil {
		return err
	}
	tpl, err := dgwsend.ParseTemplate(tplRaw)
	if err != nil {
		return err
	}

	convID := *conversation
	if *newConv {
		if convID, err = dgwsend.NewRequestID(); err != nil {
			return err
		}
	}

	reqID, err := dgwsend.NewRequestID()
	if err != nil {
		return err
	}

	body, err := tpl.Build(*prompt, convIDOrTemplate(convID, tpl), reqID)
	if err != nil {
		return err
	}
	effectiveConv := convIDOrTemplate(convID, tpl)

	fmt.Fprintf(os.Stderr, "dgwsend: prompt=%q conversation=%s request=%s body=%d bytes\n",
		*prompt, effectiveConv, reqID, len(body))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := dial(ctx, *token, effectiveConv)
	if err != nil {
		return err
	}
	defer conn.Close()

	// The gateway needs the stream established before it will route data.
	estab := estabPayload(effectiveConv)
	if err := conn.WriteMessage(websocket.BinaryMessage, buildFrame(dgw.FrameStreamGroupEstabStream, estab, false)); err != nil {
		return fmt.Errorf("write estab: %w", err)
	}
	fmt.Fprintln(os.Stderr, "dgwsend: sent EstabStream")

	// The prompt frame: JSON envelope {req-id, payload: base64(proto)}.
	env, _ := json.Marshal(map[string]string{
		"req-id":  reqID,
		"payload": base64.StdEncoding.EncodeToString(body),
	})
	for _, f := range buildFrames(dgw.FrameStreamGroupData, env) {
		if err := conn.WriteMessage(websocket.BinaryMessage, f); err != nil {
			return fmt.Errorf("write data: %w", err)
		}
	}
	fmt.Fprintln(os.Stderr, "dgwsend: sent Data (prompt)")

	deadline := time.Now().Add(*timeout)
	_ = conn.SetReadDeadline(deadline)
	asm := ectostream.NewAssembler()
	var text []string
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if len(text) > 0 || asm.SawFull() {
				printReply(asm, join(text))
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}

		if *raw {
			fmt.Printf("%s\n", hex.EncodeToString(msg))
		}

		frame, err := dgw.DecodeWireFrame(msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dgwsend: undecodable frame: %v\n", err)
			continue
		}
		// Answer the keep-alive, otherwise the gateway drops the stream.
		if frame.Type == dgw.FramePing {
			_ = conn.WriteMessage(websocket.BinaryMessage, buildFrame(dgw.FramePong, nil, false))
			continue
		}
		// A Data frame that asks for an ack must get one.
		if frame.RequiresAck && frame.Type == dgw.FrameStreamGroupData {
			_ = conn.WriteMessage(websocket.BinaryMessage, buildAck(frame.AckID))
		}

		deltas, err := ectostream.DecodeResponse(msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dgwsend: %v\n", err)
			continue
		}
		for _, d := range deltas {
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", d.Kind, preview(d.Text, 80))
			if d.Kind == ectostream.KindUnified {
				if err := asm.Apply(d.Text); err != nil {
					fmt.Fprintf(os.Stderr, "dgwsend: assemble: %v\n", err)
				}
			}
			if d.Kind == ectostream.KindText {
				// A "full" frame replaces the answer; the server sends one at
				// the end carrying the complete text, so appending every chunk
				// would print the answer twice.
				if d.Replace {
					text = text[:0]
				}
				text = append(text, d.Text)
			}
			if d.Final {
				printReply(asm, join(text))
				return nil
			}
		}
	}
}

// printReply prefers the assembled document, which handles sections that are not
// answer text, inline-entity markers and citations. The chunk join is the
// fallback for a reply that carried no unified payload.
func printReply(asm *ectostream.Assembler, chunks string) {
	answer := chunks
	if asm.SawFull() {
		if a := asm.AnswerText(); a != "" {
			answer = a
		}
	}
	fmt.Printf("\n--- reply ---\n%s\n", answer)

	if p := asm.ProgressText(); p != "" {
		fmt.Printf("\n--- progress ---\n%s\n", p)
	}
	if media := asm.Media(); len(media) > 0 {
		fmt.Printf("\n--- media (%d) ---\n", len(media))
		for i, m := range media {
			dims := ""
			if m.Width > 0 {
				dims = fmt.Sprintf(" %dx%d", m.Width, m.Height)
			}
			fmt.Printf("%d. %s%s\n   %s\n", i+1, m.MimeType, dims, m.URL)
			if m.PreviewURL != "" && m.PreviewURL != m.URL {
				fmt.Printf("   preview: %s\n", m.PreviewURL)
			}
			if m.Source != "" {
				fmt.Printf("   source:  %s\n", m.Source)
			}
		}
	}

	if cites := asm.Citations(); len(cites) > 0 {
		fmt.Printf("\n--- sources (%d) ---\n", len(cites))
		for i, c := range cites {
			fmt.Printf("%d. %s\n   %s\n", i+1, c.Title, c.URL)
		}
	}
}

func convIDOrTemplate(id string, tpl *dgwsend.PromptTemplate) string {
	if id != "" {
		return id
	}
	return tpl.ConversationID()
}

// dial opens the gateway websocket. The query string is exactly what the client
// bundle builds; appId and authtype are constants observed there.
func dial(ctx context.Context, token, convID string) (*websocket.Conn, error) {
	q := url.Values{}
	q.Set("x-dgw-appid", "1522763855472543")
	q.Set("x-dgw-appversion", "1.0.0")
	q.Set("x-dgw-authtype", "15:0")
	q.Set("x-dgw-version", "5")
	q.Set("x-dgw-uuid", "0")
	q.Set("x-dgw-tier", "prod")
	q.Set("Authorization", token)
	q.Set("x-dgw-app-origin", "meta.ai")

	u := "wss://gateway.meta.ai/ws/clippy?" + q.Encode()

	h := map[string][]string{"Origin": {"https://meta.ai"}}
	if convID != "" {
		h["x-dgw-app-x-ecto-conversation-id"] = []string{convID}
	}

	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := d.DialContext(ctx, u, h)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial: %w (status %s)", err, resp.Status)
		}
		return nil, err
	}
	return conn, nil
}

// estabPayload is the JSON the client sends to establish a conversation stream.
func estabPayload(convID string) []byte {
	b, _ := json.Marshal(map[string]string{
		"x-dgw-app-x-ecto-conversation-id": convID,
		"x-dgw-app-client-payload-type":    "PROTO_INSIDE_JSON",
	})
	return b
}

// buildFrame assembles the wire layout verified from recordings:
// type(1) streamId(2 LE) len(3 LE) [ack(2 LE)] body, where len counts everything
// after the 6-byte header.
func buildFrame(t dgw.FrameType, body []byte, withAck bool) []byte {
	n := len(body)
	if withAck {
		n += 2
	}
	out := []byte{byte(t), 0, 0, byte(n), byte(n >> 8), byte(n >> 16)}
	if withAck {
		out = append(out, 0, 0)
	}
	return append(out, body...)
}

// buildFrames returns the Data frame for a body. The prompt frame carries an ack
// field, as every recorded outbound Data frame did.
func buildFrames(t dgw.FrameType, body []byte) [][]byte {
	return [][]byte{buildFrame(t, body, true)}
}

// buildAck answers a Data frame that asked for acknowledgement.
func buildAck(ackID uint32) []byte {
	n := 2
	out := []byte{byte(dgw.FrameStreamGroupAck), 0, 0, byte(n), byte(n >> 8), byte(n >> 16)}
	return append(out, byte(ackID), byte(ackID>>8))
}

func preview(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func join(parts []string) string {
	out := ""
	for _, p := range parts {
		out += p
	}
	return out
}
