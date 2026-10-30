//go:build wasm_codec

package codec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental/wazerotest"

	"github.com/me/metaai2api/internal/meta/dgw"
)

// These tests exercise the tagged bridge without dgwcppbridge.wasm, which this
// repository does not ship. They drive a fake api.Module that exports the
// module's function names with test bodies.
//
// The bodies are NOT a DGW codec: they do not encode or decode a frame and they
// answer with constants a test pins. They exist to prove the bridge binds every
// export, copies bytes in and out of guest memory with the right lengths and
// offsets, honours the 8-byte output struct, and reports a missing export or a
// non-success return code as a typed error. Real frame round-tripping needs the
// real module; see testdata/README.md.

const (
	fakePageSize = 64 * 1024
	fakePages    = 4
	// factoryPtr is where the fake encoders put returned frame bytes. It must
	// stay clear of the fixture malloc's bump moves (see bumpAllocator).
	factoryPtr = 65536
	// bumpStart and bumpEnd bound the fixture allocator, which hands every
	// caller a distinct pointer so a payload copy cannot land on the output
	// struct.
	bumpStart = 1024
	bumpEnd   = 32768
)

type fakeModule struct {
	mod *wazerotest.Module
	mem *wazerotest.Memory
}

// newFakeModule builds a module exporting every symbol bind() wants. overrides
// replace a body; omits drop an export entirely.
func newFakeModule(t *testing.T, overrides map[string]any, omits ...string) *fakeModule {
	t.Helper()
	mem := wazerotest.NewMemory(fakePages * fakePageSize)
	drop := make(map[string]bool, len(omits))
	for _, name := range omits {
		drop[name] = true
	}

	var fns []*wazerotest.Function
	for _, name := range allSymbols() {
		if drop[name] {
			continue
		}
		var body any
		if overrides != nil {
			body = overrides[name]
		}
		if body == nil {
			body = defaultBody(name, mem)
		}
		fn := wazerotest.NewFunction(body)
		fn.ExportNames = []string{name}
		fns = append(fns, fn)
	}
	return &fakeModule{mod: wazerotest.NewModule(mem, fns...), mem: mem}
}

// allSymbols is every export bind() resolves.
func allSymbols() []string {
	return append(append([]string{}, RequiredSymbols...),
		symbolFrameAt, symbolFrameType, symbolDrainReason, symbolStreamID,
		symbolEstabParams, symbolEstabParamsLen, symbolDataBytes, symbolDataLen,
		symbolRequiresAck, symbolAckID,
	)
}

// defaultBody is a well-behaved stand-in for one export.
func defaultBody(name string, mem *wazerotest.Memory) any {
	switch name {
	case SymbolMalloc:
		return bumpAllocator(fakePages * fakePageSize)
	case SymbolFree:
		return func(context.Context, api.Module, uint32) {}
	case SymbolDecode:
		// Report one frame at pointer 0, count 1, all input consumed.
		return func(_ context.Context, _ api.Module, in, inLen, framesOut, countOut, consumedOut uint32) uint32 {
			mem.WriteUint32Le(framesOut, 0)
			mem.WriteUint32Le(countOut, 1)
			mem.WriteUint32Le(consumedOut, inLen)
			return returnGrouped
		}
	case symbolFrameAt:
		return func(context.Context, api.Module, uint32, uint32) uint32 { return 0 }
	case symbolFrameType:
		return func(context.Context, api.Module, uint32) uint32 { return uint32(dgw.FramePing) }
	case symbolDrainReason:
		return func(context.Context, api.Module, uint32) uint32 { return 0 }
	case symbolStreamID:
		return func(context.Context, api.Module, uint32) uint32 { return 0 }
	case symbolEstabParams, symbolDataBytes, symbolAckID:
		return func(context.Context, api.Module, uint32) uint32 { return 0 }
	case symbolEstabParamsLen, symbolDataLen:
		return func(context.Context, api.Module, uint32) uint32 { return 0 }
	case symbolRequiresAck:
		return func(context.Context, api.Module, uint32) uint32 { return 0 }
	}
	// Every encoder reports success. Its out-pointer argument is the last one
	// in each signature: empty/deauth/ping/pong take (out), endData/drain take
	// (id, out), estab takes (id, ptr, len, out, outLen), ack (id, ack, out,
	// outLen) and data (id, ptr, len, req, ack, out, outLen).
	return successEncoder(name)
}

// bumpAllocator hands out distinct 8-byte-aligned blocks, like a real malloc:
// a fixture that returned one constant pointer would alias the payload, the
// output struct and the encoded body.
func bumpAllocator(memorySize uint32) any {
	next := uint32(bumpStart)
	return func(_ context.Context, _ api.Module, size uint32) uint32 {
		p := next
		next += (size + 7) &^ 7
		if next >= bumpEnd || next > memorySize {
			return 0 // the module's out-of-memory answer
		}
		return p
	}
}

// successEncoder writes a zero-length result into the struct at out.
func successEncoder(name string) any {
	write := func(mem *wazerotest.Memory, out uint32) uint32 {
		mem.WriteUint32Le(out, 0)
		mem.WriteUint32Le(out+4, 0)
		return returnGrouped
	}
	// The fake writes through the module memory, which it reaches from the
	// api.Module argument, so one body shape per arity is enough.
	switch name {
	case SymbolEncodeEmpty, SymbolEncodeDeauth, SymbolEncodePing, SymbolEncodePong:
		return func(_ context.Context, m api.Module, out uint32) uint32 {
			return write(m.Memory().(*wazerotest.Memory), out)
		}
	case SymbolEncodeEndData, SymbolEncodeDrain:
		return func(_ context.Context, m api.Module, _, out uint32) uint32 {
			return write(m.Memory().(*wazerotest.Memory), out)
		}
	case SymbolEncodeEstab:
		return func(_ context.Context, m api.Module, _, _, _, out, _ uint32) uint32 {
			return write(m.Memory().(*wazerotest.Memory), out)
		}
	case SymbolEncodeAck:
		return func(_ context.Context, m api.Module, _, _, out, _ uint32) uint32 {
			return write(m.Memory().(*wazerotest.Memory), out)
		}
	case SymbolEncodeData:
		return func(_ context.Context, m api.Module, _, _, _, _, _, out, _ uint32) uint32 {
			return write(m.Memory().(*wazerotest.Memory), out)
		}
	}
	panic("no fake body for " + name)
}

// newBridgeForTest wraps a fake module and runs the real bind().
func newBridgeForTest(t *testing.T, f *fakeModule) *dgwBridge {
	t.Helper()
	b := &dgwBridge{mod: f.mod, memory: f.mem}
	if err := b.bind(); err != nil {
		t.Fatalf("bind: %v", err)
	}
	return b
}

func TestBindRejectsMissingEncoderSymbol(t *testing.T) {
	b := &dgwBridge{
		mod:    newFakeModule(t, nil, SymbolEncodeData).mod,
		memory: nil,
	}
	b.memory = b.mod.Memory()
	err := b.bind()
	var missing *MissingSymbolError
	if !errorsAs(err, &missing) {
		t.Fatalf("bind() error = %v, want *MissingSymbolError", err)
	}
	if missing.Symbol != SymbolEncodeData {
		t.Fatalf("missing symbol = %q, want %q", missing.Symbol, SymbolEncodeData)
	}
}

func TestBindRejectsMissingAccessorSymbol(t *testing.T) {
	b := &dgwBridge{mod: newFakeModule(t, nil, symbolDataBytes).mod}
	b.memory = b.mod.Memory()
	err := b.bind()
	var missing *MissingSymbolError
	if !errorsAs(err, &missing) {
		t.Fatalf("bind() error = %v, want *MissingSymbolError", err)
	}
	if missing.Symbol != symbolDataBytes {
		t.Fatalf("missing symbol = %q, want %q", missing.Symbol, symbolDataBytes)
	}
}

func TestExportsListsFakeModuleNames(t *testing.T) {
	b := newBridgeForTest(t, newFakeModule(t, nil))
	names, err := b.Exports(context.Background())
	if err != nil {
		t.Fatalf("Exports: %v", err)
	}
	if len(names) != len(allSymbols()) {
		t.Fatalf("Exports() = %d names, want %d", len(names), len(allSymbols()))
	}
}

func TestEncodeFrameRefusesStreamIDOnBareFrame(t *testing.T) {
	b := newBridgeForTest(t, newFakeModule(t, nil))
	_, err := b.EncodeFrame(context.Background(), dgw.Frame{Type: dgw.FramePing, StreamID: 3})
	if err == nil {
		t.Fatal("EncodeFrame accepted a stream id on a Ping")
	}
}

func TestEncodeFrameRefusesStreamIDAboveTheField(t *testing.T) {
	b := newBridgeForTest(t, newFakeModule(t, nil))
	_, err := b.EncodeFrame(context.Background(), dgw.Frame{
		Type:     dgw.FrameStreamGroupData,
		StreamID: dgw.MaxStreamID + 1,
	})
	if err == nil {
		t.Fatal("EncodeFrame accepted a stream id the field cannot hold")
	}
}

func TestEncodeFrameReportsReturnCode(t *testing.T) {
	overrides := map[string]any{
		SymbolEncodeData: func(context.Context, api.Module, uint32, uint32, uint32, uint32, uint32, uint32, uint32) uint32 {
			return returnInvalidArgument
		},
	}
	b := newBridgeForTest(t, newFakeModule(t, overrides))
	_, err := b.EncodeFrame(context.Background(), dgw.Frame{
		Type:       dgw.FrameStreamGroupData,
		StreamID:   1,
		StreamData: []byte("x"),
	})
	var rc *ReturnCodeError
	if !errorsAs(err, &rc) {
		t.Fatalf("EncodeFrame error = %v, want *ReturnCodeError", err)
	}
	if rc.Code != returnInvalidArgument {
		t.Fatalf("code = %d, want %d", rc.Code, returnInvalidArgument)
	}
	if !errors.Is(err, errBadArgument) {
		t.Fatalf("error %v should match errBadArgument", err)
	}
}

func TestEncodeFrameCopiesPayloadInAndResultOut(t *testing.T) {
	// The fake Data encoder records the bytes it was handed and returns a
	// fixed frame body, so the test can prove copyIn and readOut.
	payload := []byte(`{"probe":"payload"}`)
	body := []byte{0x80, 0x00, 0x01, 0x00, 0x7f}
	overrides := map[string]any{
		SymbolEncodeData: func(ctx context.Context, m api.Module, streamID, ptr, n, _, _, out, outLen uint32) uint32 {
			if streamID != 7 {
				t.Errorf("stream id = %d, want 7", streamID)
			}
			got, ok := m.Memory().Read(ptr, n)
			if !ok {
				t.Errorf("payload at %d, %d outside memory", ptr, n)
				return returnInvalidArgument
			}
			if string(got) != string(payload) {
				t.Errorf("payload = %q, want %q", got, payload)
			}
			mem := m.Memory().(*wazerotest.Memory)
			for i, v := range body {
				mem.WriteByte(factoryPtr+uint32(i), v)
			}
			mem.WriteUint32Le(out, factoryPtr)
			mem.WriteUint32Le(out+4, uint32(len(body)))
			if outLen != out+4 {
				t.Errorf("outLen = %d, want %d", outLen, out+4)
			}
			return returnGrouped
		},
	}
	b := newBridgeForTest(t, newFakeModule(t, overrides))
	raw, err := b.EncodeFrame(context.Background(), dgw.Frame{
		Type:       dgw.FrameStreamGroupData,
		StreamID:   7,
		StreamData: payload,
	})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if string(raw) != string(body) {
		t.Fatalf("encoded = %x, want %x", raw, body)
	}
}

func TestDecodeRefusesTruncatedInput(t *testing.T) {
	b := newBridgeForTest(t, newFakeModule(t, nil))
	if _, err := b.DecodeAll(context.Background(), nil); err == nil {
		t.Fatal("DecodeAll accepted an empty payload")
	}
}

func TestDecodeReadsTheSiteStructLayout(t *testing.T) {
	// processData in 2umt4-28uxtjh.js allocates one 16-byte struct and passes
	// r, r+4, (r+4)+4, ((r+4)+4)+4: +0 frames pointer (it reads HEAPU32[r/4]),
	// +4 frame count, +8 bytes consumed. A decode that put the count where the
	// site puts the consumed length would then count frames from uninitialised
	// memory, so the fake writes each field where the site expects it and the
	// bridge must read it back from there.
	overrides := map[string]any{
		SymbolDecode: func(_ context.Context, _ api.Module, in, inLen, a, b2, c uint32) uint32 {
			if inLen != 3 {
				t.Errorf("decode input length = %d, want 3", inLen)
			}
			if a == 0 || b2 != a+4 || c != a+8 {
				t.Errorf("struct out pointers = %d, %d, %d; want a contiguous triple", a, b2, c)
				return returnInvalidArgument
			}
			return returnGrouped
		},
	}
	b := newBridgeForTest(t, newFakeModule(t, overrides))
	if _, err := b.DecodeAll(context.Background(), []byte{1, 2, 3}); err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
}

func TestEncodePongRejectsANonPing(t *testing.T) {
	b := newBridgeForTest(t, newFakeModule(t, nil))
	if _, err := b.EncodePong(dgw.Frame{Type: dgw.FrameStreamGroupData}); err == nil {
		t.Fatal("EncodePong accepted a non-Ping frame")
	}
}

// errorsAs is errors.As for a pointer target, without the generic dance.
func errorsAs[T any](err error, target *T) bool {
	for err != nil {
		if e, ok := err.(T); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestNewRejectsANonWasmFileWithoutPanicking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AssetName)
	if err := os.WriteFile(path, []byte("this is not a wasm module"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(AssetEnv, path)

	c, err := New()
	if err == nil {
		t.Fatalf("New() = %#v, nil; want an error for a non-wasm file", c)
	}
	if errors.Is(err, errMissingAsset) {
		t.Fatalf("New() error = %v; a present but invalid file is not a missing asset", err)
	}
}

func TestNewReportsAMissingOverride(t *testing.T) {
	t.Setenv(AssetEnv, filepath.Join(t.TempDir(), "absent.wasm"))
	if _, err := New(); !errors.Is(err, errMissingAsset) {
		t.Fatalf("New() error = %v, want backend.ErrUnavailable", err)
	}
}

func TestEncodersPassTheSiteArgumentOrder(t *testing.T) {
	// Each encoder records the arguments it receives; the expected shape is the
	// site's call site, not a guess. A swapped pair here would silently
	// mislabel every frame on the wire.
	var got struct {
		empty, deauth, drain, ping, pong []uint32
		ack, data, endData, estab        []uint32
	}
	overrides := map[string]any{
		SymbolEncodeEmpty: func(_ context.Context, _ api.Module, out uint32) uint32 {
			got.empty = []uint32{out}
			return returnGrouped
		},
		SymbolEncodeDeauth: func(_ context.Context, _ api.Module, out uint32) uint32 {
			got.deauth = []uint32{out}
			return returnGrouped
		},
		SymbolEncodeDrain: func(_ context.Context, _ api.Module, streamID, out, outLen uint32) uint32 {
			got.drain = []uint32{streamID, out, outLen}
			return returnGrouped
		},
		SymbolEncodePing: func(_ context.Context, _ api.Module, out uint32) uint32 {
			got.ping = []uint32{out}
			return returnGrouped
		},
		SymbolEncodePong: func(_ context.Context, _ api.Module, out uint32) uint32 {
			got.pong = []uint32{out}
			return returnGrouped
		},
		SymbolEncodeAck: func(_ context.Context, _ api.Module, streamID, ackID, out, outLen uint32) uint32 {
			got.ack = []uint32{streamID, ackID, out, outLen}
			return returnGrouped
		},
		SymbolEncodeData: func(_ context.Context, _ api.Module, streamID, ptr, n, req, ack, out, outLen uint32) uint32 {
			got.data = []uint32{streamID, n, req, ack, out, outLen}
			return returnGrouped
		},
		SymbolEncodeEndData: func(_ context.Context, _ api.Module, streamID, out, outLen uint32) uint32 {
			got.endData = []uint32{streamID, out, outLen}
			return returnGrouped
		},
		SymbolEncodeEstab: func(_ context.Context, _ api.Module, streamID, ptr, n, out, outLen uint32) uint32 {
			got.estab = []uint32{streamID, n, out, outLen}
			return returnGrouped
		},
	}
	b := newBridgeForTest(t, newFakeModule(t, overrides))
	ctx := context.Background()

	for _, f := range []dgw.Frame{
		{Type: dgw.FrameEmpty},
		{Type: dgw.FrameDeauth},
		{Type: dgw.FrameDrain, StreamID: 5},
		{Type: dgw.FramePing},
		{Type: dgw.FramePong},
		{Type: dgw.FrameStreamGroupAck, StreamID: 6, AckID: 77},
		{Type: dgw.FrameStreamGroupData, StreamID: 7, StreamData: []byte("xy"), RequiresAck: false},
		{Type: dgw.FrameStreamGroupEndOfData, StreamID: 8},
		{Type: dgw.FrameStreamGroupEstabStream, StreamID: 9, StreamData: []byte("z")},
	} {
		if _, err := b.EncodeFrame(ctx, f); err != nil {
			t.Fatalf("EncodeFrame(%v): %v", f.Type, err)
		}
	}

	for name, args := range map[string][]uint32{
		"empty": got.empty, "deauth": got.deauth, "ping": got.ping, "pong": got.pong,
	} {
		// The site's encodePing/encodePong/encodeEmpty/encodeDeauth take the
		// output pointer alone and read the pair as HEAPU32[out/4], [+1].
		if args[0] == 0 {
			t.Errorf("%s got a null output pointer", name)
		}
	}
	if got.drain[0] != 5 {
		t.Errorf("drain stream id = %d, want 5", got.drain[0])
	}
	if got.drain[2] != got.drain[1]+4 {
		t.Errorf("drain outLen = %d, want out+4 (%d)", got.drain[2], got.drain[1]+4)
	}
	if got.ack[0] != 6 || got.ack[1] != 77 {
		t.Errorf("ack = (stream %d, ack %d), want (6, 77)", got.ack[0], got.ack[1])
	}
	if got.data[0] != 7 || got.data[1] != 2 || got.data[2] != 0 || got.data[3] != 0 {
		t.Errorf("data = %v, want [7 2 0 0 ...]", got.data[:4])
	}
	if got.endData[0] != 8 {
		t.Errorf("endOfData stream id = %d, want 8", got.endData[0])
	}
	if got.estab[0] != 9 || got.estab[1] != 1 {
		t.Errorf("estab = %v, want stream 9 len 1", got.estab[:2])
	}
}
