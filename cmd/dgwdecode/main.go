// Command dgwdecode reads captured DGW websocket frames and prints what they
// carry.
//
// It exists so a capture can be inspected without sending it anywhere: paste the
// hex or base64 of a frame, or pipe a whole capture file, and it prints the frame
// header and every delta the reply contains.
//
// Input forms accepted per input line (leading and trailing space ignored):
//
//	<hex>                      a frame as hex
//	b64:<base64>               a frame as base64
//	<hex-or-b64>  # comment    trailing text after whitespace is ignored
//
// Lines that are not frames are reported on stderr and skipped, so a
// tab-separated capture with a header row can be fed in whole: the b64 column is
// picked out automatically.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/me/metaai2api/internal/meta/dgw"
	"github.com/me/metaai2api/internal/meta/dgw/ectostream"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "dgwdecode:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("dgwdecode", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "also print the frame header and decoded protobuf field names")
	asJSON := fs.Bool("json", false, "emit deltas as JSON lines")
	if err := fs.Parse(args); err != nil {
		return err
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	lineNo := 0
	decoded := 0
	for in.Scan() {
		lineNo++
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}

		raw, ok := extractFrame(line)
		if !ok {
			continue // header row or unrelated text
		}

		frame, err := dgw.DecodeWireFrame(raw)
		if err != nil {
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		decoded++

		if *verbose {
			printFrame(frame)
		}

		deltas, err := ectostream.DecodeResponse(raw)
		if err != nil {
			// A control frame carrying JSON ({code:200}, stream headers) is not
			// a Response. That is expected, not a decode failure, so it is only
			// noise when -v asked for headers.
			if errors.Is(err, ectostream.ErrNotResponse) {
				if *verbose {
					fmt.Fprintf(os.Stderr, "line %d: no response payload (%v)\n", lineNo, err)
				}
				continue
			}
			fmt.Fprintf(os.Stderr, "line %d: %v\n", lineNo, err)
			continue
		}
		for _, d := range deltas {
			if *asJSON {
				emitJSON(d)
				continue
			}
			fmt.Println(formatDelta(d))
		}
	}
	if err := in.Err(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "dgwdecode: %d frame(s) decoded\n", decoded)
	return nil
}

// extractFrame pulls frame bytes out of one input line.
//
// A line from the capture format is tab-separated with the base64 in the last
// column, so splitting on tabs is enough; a bare hex or b64:<...> line is taken
// whole.
func extractFrame(line string) ([]byte, bool) {
	fields := strings.Split(line, "\t")
	candidate := strings.TrimSpace(fields[len(fields)-1])

	if strings.HasPrefix(candidate, "b64:") {
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(candidate[4:]))
		return b, err == nil && looksLikeFrame(b)
	}

	// A bare hex string.
	if b, err := hex.DecodeString(strings.NewReplacer(" ", "", ":", "", "-", "").Replace(candidate)); err == nil {
		if looksLikeFrame(b) {
			return b, true
		}
	}

	// A bare base64 string.
	if b, err := base64.StdEncoding.DecodeString(candidate); err == nil && looksLikeFrame(b) {
		return b, true
	}

	return nil, false
}

// looksLikeFrame reports whether b could be a DGW frame: long enough for a
// header, a known type byte, and a length field that matches the size.
func looksLikeFrame(b []byte) bool {
	if len(b) < 6 {
		return false
	}
	if !dgw.FrameType(b[0]).Known() {
		return false
	}
	declared := int(b[3]) | int(b[4])<<8 | int(b[5])<<16
	return declared == len(b)-6
}

func printFrame(f dgw.Frame) {
	fmt.Printf("frame type=%s stream=%d ack=%d requiresAck=%v bytes=%d\n",
		f.Type, f.StreamID, f.AckID, f.RequiresAck, len(f.Raw))
	if body := f.StreamData; len(body) > 0 {
		if json.Valid(body) {
			fmt.Printf("  body(json): %s\n", truncate(string(body), 300))
		} else {
			fmt.Printf("  body(proto): %d bytes\n", len(body))
		}
	}
}

func formatDelta(d ectostream.Delta) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-10s", d.Kind)
	if d.Path != "" {
		fmt.Fprintf(&b, " path=%q", d.Path)
	}
	if d.Op != "" {
		fmt.Fprintf(&b, " op=%q", d.Op)
	}
	if d.Final {
		b.WriteString(" final")
	}
	if d.Text != "" {
		fmt.Fprintf(&b, " %q", d.Text)
	}
	return b.String()
}

func emitJSON(d ectostream.Delta) {
	b, err := json.Marshal(d)
	if err != nil {
		return
	}
	os.Stdout.Write(b)
	os.Stdout.Write([]byte("\n"))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
