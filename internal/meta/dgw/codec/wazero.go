// The wazero bridge from dgwcppbridge.wasm into dgw.Codec.
//
// This file builds only with -tags wasm_codec. The module is embedded from the
// codec directory (see asset.go), which `make fetch-wasm` fills in; without the
// module the tagged build fails at compile time, by design, while the default
// build stays green through unavailable.go.
//
// The call sequences are transcribed from the site's JavaScript:
//
//	02h3llx9n0qf1.js  loader: M="dgwcppbridge.wasm", HEAPU8, _malloc, the
//	                  __DgwCodec*/__get* export table, __wasm_call_ctors
//	2umt4-28uxtjh.js  call sites: encodeData, encodeAck, encodeDrain,
//	                  encodeEstablishStream, encodeEndOfData, encodePing,
//	                  encodePong, processData (decode)
//
// Nothing here guesses an ABI: every export the bridge calls is looked up first
// and a missing one is a MissingSymbolError, never a panic and never a silent
// zero.

//go:build wasm_codec

package codec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/me/metaai2api/internal/meta/dgw"
)

const (
	// builtIn reports whether this build has the bridge. True here.
	builtIn = true

	// Return codes from DgwCodecReturnCode_* in the site's bundle.
	//
	// A Successful decode splits the input on a newline; the site's decode
	// wrapper marks whether the trailing segment is a complete frame with bit 1
	// (0 = complete, 1 = not), so return 1 is also success there.
	returnGrouped  = 0
	returnSuccess1 = 1
	// returnInvalidArgument is DgwCodecReturnCode_InvalidArgument.
	returnInvalidArgument = 4

	// wasmPage is the size of a WebAssembly memory page.
	wasmPage = 64 * 1024

	// maxBuffer is the largest buffer the bridge will allocate in the module.
	// The site's frames are protobuf messages of a few KiB; anything near this
	// bound is a bug or an attack, not a frame.
	maxBuffer = 1 << 22
)

// DgwCodecReturnCode names the module's error codes, for diagnostics.
var codecReturnCodes = map[uint32]string{
	0:  "Success",
	1:  "Success1",
	2:  "UnexpectedError",
	3:  "NotEnoughMemory",
	4:  "InvalidArgument",
	5:  "DeserializeError",
	6:  "SerializeError",
	7:  "StreamIdMismatch",
	8:  "SequenceNumberMismatch",
	9:  "BufferTooSmall",
	10: "InvalidStreamId",
	11: "TruncatedBuffer",
}

// MissingSymbolError reports a module that does not export a symbol the bridge
// calls. The frame wire format is not guessed here, so an unexpected module is
// refused instead of half-driven.
type MissingSymbolError struct {
	// Symbol is the export name that was not found.
	Symbol string
}

func (e *MissingSymbolError) Error() string {
	return fmt.Sprintf("dgw codec: module does not export %s", e.Symbol)
}

// ReturnCodeError reports a non-success DgwCodecReturnCode from an exported
// call.
type ReturnCodeError struct {
	// Op names the bridge operation that failed.
	Op string
	// Code is the raw return value.
	Code uint32
}

func (e *ReturnCodeError) Error() string {
	name, ok := codecReturnCodes[e.Code]
	if !ok {
		name = "Unknown"
	}
	return fmt.Sprintf("dgw codec: %s returned %s (%d)", e.Op, name, e.Code)
}

// Is reports which failures the caller can treat as a bad argument rather than
// a module fault.
func (e *ReturnCodeError) Is(target error) bool {
	return target == errBadArgument && e.Code == returnInvalidArgument
}

// errNoModule reports that no wasm bytes were found at any known location. It
// matches backend.ErrUnavailable so callers use one check.
var errNoModule = fmt.Errorf("%w: no %s found", errMissingAsset, AssetName)

// dgwBridge is the concrete Codec. It owns one wazero runtime and module.
type dgwBridge struct {
	rt     wazero.Runtime
	mod    api.Module
	memory api.Memory

	malloc api.Function
	free   api.Function

	encodeEmpty   api.Function
	encodeDeauth  api.Function
	encodeDrain   api.Function
	encodePing    api.Function
	encodePong    api.Function
	encodeAck     api.Function
	encodeData    api.Function
	encodeEndData api.Function
	encodeEstab   api.Function
	decode        api.Function

	frameAt        api.Function
	frameType      api.Function
	drainReason    api.Function
	streamID       api.Function
	estabParams    api.Function
	estabParamsLen api.Function
	dataBytes      api.Function
	dataLen        api.Function
	requiresAck    api.Function
	ackID          api.Function

	closeOnce sync.Once
	closeErr  error
}

// New loads and instantiates dgwcppbridge.wasm and binds every symbol the bridge
// calls. A missing module is backend.ErrUnavailable; a module missing a symbol
// is a MissingSymbolError. Neither is a panic.
func New() (Codec, error) {
	path, err := AssetPath()
	if err != nil {
		return nil, err
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", errMissingAsset, path, err)
	}

	ctx := context.Background()
	rt := wazero.NewRuntime(ctx)
	bridge := &dgwBridge{rt: rt}

	if err := bridge.instantiate(ctx, wasm); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	if err := bridge.bind(); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}
	return bridge, nil
}

// instantiate compiles and instantiates the module, runs its constructors and
// verifies it exports the memory the loader reads as HEAPU8.
func (b *dgwBridge) instantiate(ctx context.Context, wasm []byte) error {
	mod, err := b.rt.InstantiateWithConfig(ctx, wasm, wazero.NewModuleConfig().
		WithName(AssetName).
		WithStartFunctions())
	if err != nil {
		return fmt.Errorf("dgw codec: instantiate %s: %w", AssetName, err)
	}
	b.mod = mod

	ctor := mod.ExportedFunction("___wasm_call_ctors")
	if ctor == nil {
		return &MissingSymbolError{Symbol: "___wasm_call_ctors"}
	}
	if _, err := ctor.Call(ctx); err != nil {
		return fmt.Errorf("dgw codec: ___wasm_call_ctors: %w", err)
	}

	mem := mod.Memory()
	if mem == nil {
		return errors.New("dgw codec: module exports no memory")
	}
	b.memory = mem
	return nil
}

// bind resolves every export. The first missing one is reported; the rest of the
// table is not consulted, because a module that is missing one is not the
// module the bridge was written against.
func (b *dgwBridge) bind() error {
	e := b.mod.ExportedFunction
	b.malloc, b.free = e(SymbolMalloc), e(SymbolFree)
	b.encodeEmpty = e(SymbolEncodeEmpty)
	b.encodeDeauth = e(SymbolEncodeDeauth)
	b.encodeDrain = e(SymbolEncodeDrain)
	b.encodePing = e(SymbolEncodePing)
	b.encodePong = e(SymbolEncodePong)
	b.encodeAck = e(SymbolEncodeAck)
	b.encodeData = e(SymbolEncodeData)
	b.encodeEndData = e(SymbolEncodeEndData)
	b.encodeEstab = e(SymbolEncodeEstab)
	b.decode = e(SymbolDecode)

	b.frameAt = e(symbolFrameAt)
	b.frameType = e(symbolFrameType)
	b.drainReason = e(symbolDrainReason)
	b.streamID = e(symbolStreamID)
	b.estabParams = e(symbolEstabParams)
	b.estabParamsLen = e(symbolEstabParamsLen)
	b.dataBytes = e(symbolDataBytes)
	b.dataLen = e(symbolDataLen)
	b.requiresAck = e(symbolRequiresAck)
	b.ackID = e(symbolAckID)

	for _, pair := range []struct {
		name string
		fn   api.Function
	}{
		{SymbolMalloc, b.malloc},
		{SymbolFree, b.free},
		{SymbolEncodeEmpty, b.encodeEmpty},
		{SymbolEncodeDeauth, b.encodeDeauth},
		{SymbolEncodeDrain, b.encodeDrain},
		{SymbolEncodePing, b.encodePing},
		{SymbolEncodePong, b.encodePong},
		{SymbolEncodeAck, b.encodeAck},
		{SymbolEncodeData, b.encodeData},
		{SymbolEncodeEndData, b.encodeEndData},
		{SymbolEncodeEstab, b.encodeEstab},
		{SymbolDecode, b.decode},
		{symbolFrameAt, b.frameAt},
		{symbolFrameType, b.frameType},
		{symbolDrainReason, b.drainReason},
		{symbolStreamID, b.streamID},
		{symbolEstabParams, b.estabParams},
		{symbolEstabParamsLen, b.estabParamsLen},
		{symbolDataBytes, b.dataBytes},
		{symbolDataLen, b.dataLen},
		{symbolRequiresAck, b.requiresAck},
		{symbolAckID, b.ackID},
	} {
		if pair.fn == nil {
			return &MissingSymbolError{Symbol: pair.name}
		}
	}
	return nil
}

// Exports lists the module's exported names, sorted.
func (b *dgwBridge) Exports(context.Context) ([]string, error) {
	if b.mod == nil {
		return nil, errors.New("dgw codec: module is closed")
	}
	names := make([]string, 0, 32)
	for name := range b.mod.ExportedFunctionDefinitions() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Close releases the runtime. It is safe to call more than once.
func (b *dgwBridge) Close(ctx context.Context) error {
	b.closeOnce.Do(func() {
		if b.rt != nil {
			b.closeErr = b.rt.Close(ctx)
		}
	})
	return b.closeErr
}

// CloseWith closes the codec when the caller's context ends, a convenience for
// the backend's per-connection lifetime.
func (b *dgwBridge) CloseWith(ctx context.Context) { _ = b.Close(ctx) }

// AssetPath resolves the wasm file: $DGW_CODEC_WASM first, then the module the
// repository put in the codec directory.
func AssetPath() (string, error) {
	if p := os.Getenv(AssetEnv); p != "" {
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%w: %s must be an absolute path, got %q", errMissingAsset, AssetEnv, p)
		}
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("%w: %s: %v", errMissingAsset, AssetEnv, err)
		}
		return p, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("%w: working directory: %v", errMissingAsset, err)
	}
	p := filepath.Join(cwd, compiledAssetPath)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%w: %s", errNoModule, p)
	}
	return p, nil
}

// assetPath is AssetPath with a fixed answer for AssetHint.
func assetPath() string { return compiledAssetPath }

// --- heap helpers -----------------------------------------------------------

// alloc reserves n bytes in the module and returns the pointer. Size zero still
// allocates one byte, because the module rejects a null buffer.
func (b *dgwBridge) alloc(ctx context.Context, n int) (uint32, error) {
	if n < 0 || n > maxBuffer {
		return 0, fmt.Errorf("dgw codec: buffer size %d out of range", n)
	}
	req := uint64(n)
	if req == 0 {
		req = 1
	}
	res, err := b.malloc.Call(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("dgw codec: malloc: %w", err)
	}
	if len(res) == 0 {
		return 0, errors.New("dgw codec: malloc returned no value")
	}
	ptr := uint32(res[0])
	if ptr == 0 {
		return 0, errors.New("dgw codec: malloc returned null (out of memory)")
	}
	return ptr, nil
}

// freeBuffer releases ptr, ignoring the module's void return.
func (b *dgwBridge) freeBuffer(ctx context.Context, ptr uint32) {
	if ptr == 0 || b.free == nil {
		return
	}
	if _, err := b.free.Call(ctx, uint64(ptr)); err != nil {
		// Freeing is best effort: the caller's error is the interesting one.
		return
	}
}

// copyIn writes src to a fresh allocation and returns its pointer.
func (b *dgwBridge) copyIn(ctx context.Context, src []byte) (uint32, error) {
	ptr, err := b.alloc(ctx, len(src))
	if err != nil {
		return 0, err
	}
	if len(src) > 0 && !b.memory.Write(ptr, src) {
		b.freeBuffer(ctx, ptr)
		return 0, fmt.Errorf("dgw codec: write %d bytes at %d outside module memory", len(src), ptr)
	}
	return ptr, nil
}

// readOut copies n bytes from the module at ptr.
func (b *dgwBridge) readOut(ptr, n uint32) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	buf, ok := b.memory.Read(ptr, n)
	if !ok {
		return nil, fmt.Errorf("dgw codec: read %d bytes at %d outside module memory", n, ptr)
	}
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, nil
}

// callU32 calls fn and returns its lone result as uint32.
func callU32(ctx context.Context, fn api.Function, name string, args ...uint64) (uint32, error) {
	res, err := fn.Call(ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("dgw codec: %s: %w", name, err)
	}
	if len(res) == 0 {
		return 0, fmt.Errorf("dgw codec: %s returned no value", name)
	}
	return uint32(res[0]), nil
}

// checkCode turns a return code into an error for anything that is not success.
func checkCode(op string, code uint32, ok ...uint32) error {
	for _, want := range ok {
		if code == want {
			return nil
		}
	}
	return &ReturnCodeError{Op: op, Code: code}
}

// --- dgw.Codec --------------------------------------------------------------

// Encode serialises frame. See EncodeFrame.
func (b *dgwBridge) Encode(frame dgw.Frame) ([]byte, error) {
	return b.EncodeFrame(context.Background(), frame)
}

// EncodeFrame serialises frame with the export its type names, exactly as the
// site's encode* methods do: allocate the output struct (8 bytes: data pointer
// at +0, length at +4), call the export, copy the bytes out, free both.
func (b *dgwBridge) EncodeFrame(ctx context.Context, frame dgw.Frame) ([]byte, error) {
	// Drain is the only non-group frame that names a stream (the site's
	// encodeDrain(streamId)).
	streamed := frame.Type == dgw.FrameStreamGroupData ||
		frame.Type == dgw.FrameStreamGroupEstabStream ||
		frame.Type == dgw.FrameStreamGroupAck ||
		frame.Type == dgw.FrameStreamGroupEndOfData ||
		frame.Type == dgw.FrameDrain
	if !streamed && frame.StreamID != 0 {
		return nil, fmt.Errorf("dgw codec: %v carries no stream id (got %d)", frame.Type, uint32(frame.StreamID))
	}
	if streamed && frame.StreamID > dgw.MaxStreamID {
		return nil, fmt.Errorf("dgw codec: stream id %d above %d", uint32(frame.StreamID), uint32(dgw.MaxStreamID))
	}

	switch frame.Type {
	case dgw.FrameStreamGroupData:
		return b.encodeWithPayload(ctx, b.encodeData, frame, false)
	case dgw.FrameStreamGroupEstabStream:
		return b.encodeWithPayload(ctx, b.encodeEstab, frame, true)
	case dgw.FrameStreamGroupAck:
		return b.encodeAckFrame(ctx, frame)
	case dgw.FrameStreamGroupEndOfData:
		return b.encodeBareWithStream(ctx, b.encodeEndData, frame)
	case dgw.FrameEmpty:
		return b.encodeBare(ctx, b.encodeEmpty, frame)
	case dgw.FrameDeauth:
		return b.encodeBare(ctx, b.encodeDeauth, frame)
	case dgw.FrameDrain:
		return b.encodeDrainFrame(ctx, frame)
	case dgw.FramePing:
		return b.encodeBare(ctx, b.encodePing, frame)
	case dgw.FramePong:
		return b.encodeBare(ctx, b.encodePong, frame)
	}
	return nil, fmt.Errorf("dgw codec: %v cannot be encoded", frame.Type)
}

// encodeBare drives an export whose only argument is the 8-byte output struct:
// Empty, Deauth, Ping and Pong.
func (b *dgwBridge) encodeBare(ctx context.Context, fn api.Function, frame dgw.Frame) ([]byte, error) {
	if frame.StreamID != 0 {
		return nil, fmt.Errorf("dgw codec: %v carries no stream id", frame.Type)
	}
	return b.callEncoder(ctx, frame.Type, fn, func(out uint32) []uint64 { return []uint64{uint64(out)} })
}

// encodeBareWithStream drives an export that takes a stream id then the output
// struct: StreamGroup_EndOfData.
func (b *dgwBridge) encodeBareWithStream(ctx context.Context, fn api.Function, frame dgw.Frame) ([]byte, error) {
	return b.callEncoder(ctx, frame.Type, fn, func(out uint32) []uint64 {
		return []uint64{uint64(frame.StreamID), uint64(out)}
	})
}

// encodeDrainFrame drives StreamGroup_Drain(streamID, out, outLen). The site
// calls it with the stream id first; the module's encodeDrain{streamGroupId}.
func (b *dgwBridge) encodeDrainFrame(ctx context.Context, frame dgw.Frame) ([]byte, error) {
	return b.callEncoder(ctx, frame.Type, b.encodeDrain, func(out uint32) []uint64 {
		return []uint64{uint64(frame.StreamID), uint64(out), uint64(out + 4)}
	})
}

// encodeAckFrame drives StreamGroup_Ack(streamId, ackId, out, outLen). The
// site's encodeAck(e=ackId, t=streamId) passes the stream id second, which is
// why the module's signature is (streamId, ackId).
func (b *dgwBridge) encodeAckFrame(ctx context.Context, frame dgw.Frame) ([]byte, error) {
	ackID := frame.AckID
	return b.callEncoder(ctx, frame.Type, b.encodeAck, func(out uint32) []uint64 {
		return []uint64{uint64(frame.StreamID), uint64(ackID), uint64(out), uint64(out + 4)}
	})
}

// encodeWithPayload drives the Data and EstabStream exports, which take the
// stream id, the payload pointer and length, then the output struct halves.
func (b *dgwBridge) encodeWithPayload(ctx context.Context, fn api.Function, frame dgw.Frame, estab bool) ([]byte, error) {
	payload := frame.StreamData
	if payload == nil {
		payload = frame.Payload
	}
	ptr, err := b.copyIn(ctx, payload)
	if err != nil {
		return nil, err
	}
	defer b.freeBuffer(ctx, ptr)

	if estab {
		// EstabStream(streamId, ptr, len, out, outLen): the site checks the
		// stream id fits a uint16 and the module's own check agrees.
		if frame.StreamID > math.MaxUint16 {
			return nil, fmt.Errorf("dgw codec: stream id %d above 65535", uint32(frame.StreamID))
		}
		return b.callEncoder(ctx, frame.Type, fn, func(out uint32) []uint64 {
			return []uint64{uint64(frame.StreamID), uint64(ptr), uint64(len(payload)), uint64(out), uint64(out + 4)}
		})
	}
	return b.callEncoder(ctx, frame.Type, fn, func(out uint32) []uint64 {
		return []uint64{uint64(frame.StreamID), uint64(ptr), uint64(len(payload)), 0, 0, uint64(out), uint64(out + 4)}
	})
}

// callEncoder allocates the 8-byte output struct, calls fn with the args the
// caller builds from its pointer, copies the result out and frees everything.
func (b *dgwBridge) callEncoder(ctx context.Context, typ dgw.FrameType, fn api.Function, args func(out uint32) []uint64) ([]byte, error) {
	out, err := b.alloc(ctx, 8)
	if err != nil {
		return nil, err
	}
	defer b.freeBuffer(ctx, out)

	code, err := callU32(ctx, fn, typ.String(), args(out)...)
	if err != nil {
		return nil, err
	}
	if err := checkCode(typ.String(), code, returnGrouped, returnSuccess1); err != nil {
		return nil, err
	}

	dataPtr, dataLen, err := b.readPair(out)
	if err != nil {
		return nil, err
	}
	defer b.freeBuffer(ctx, dataPtr)
	return b.readOut(dataPtr, dataLen)
}

// readPair reads the two uint32 fields of an output struct at ptr.
func (b *dgwBridge) readPair(ptr uint32) (data, length uint32, err error) {
	data, ok := b.memory.ReadUint32Le(ptr)
	if !ok {
		return 0, 0, fmt.Errorf("dgw codec: output struct at %d outside module memory", ptr)
	}
	length, ok = b.memory.ReadUint32Le(ptr + 4)
	if !ok {
		return 0, 0, fmt.Errorf("dgw codec: output length at %d outside module memory", ptr+4)
	}
	return data, length, nil
}

// Decode parses the first frame in raw. See DecodeAll.
func (b *dgwBridge) Decode(raw []byte) (dgw.Frame, error) {
	frames, err := b.DecodeAll(context.Background(), raw)
	if err != nil {
		return dgw.Frame{}, err
	}
	if len(frames) == 0 {
		return dgw.Frame{}, errors.New("dgw codec: decode produced no frames")
	}
	if len(frames) > 1 {
		return dgw.Frame{}, fmt.Errorf("dgw codec: decode produced %d frames, Decode handles one", len(frames))
	}
	return frames[0], nil
}

// DecodeAll drives __DgwCodecDecode(in, inLen, count, framePtrs, frameCount,
// consumed) as processData does and then walks the frame array with the __get*
// exports. It never parses wire bytes itself.
func (b *dgwBridge) DecodeAll(ctx context.Context, raw []byte) ([]dgw.Frame, error) {
	if len(raw) == 0 {
		return nil, errors.New("dgw codec: decode of empty payload")
	}
	ptr, err := b.copyIn(ctx, raw)
	if err != nil {
		return nil, err
	}
	defer b.freeBuffer(ctx, ptr)

	out, err := b.alloc(ctx, 16)
	if err != nil {
		return nil, err
	}
	defer b.freeBuffer(ctx, out)

	// The struct layout is the site's: +0 frames pointer (read as u32), +4
	// frame count, +8 frames pointer (read as u32), +12 bytes consumed.
	framesOut := out
	countOut := out + 4
	consumedOut := out + 8
	code, err := callU32(ctx, b.decode, "Decode",
		uint64(ptr), uint64(len(raw)),
		uint64(framesOut), uint64(countOut), uint64(consumedOut))
	if err != nil {
		return nil, err
	}
	if err := checkCode("Decode", code, returnGrouped, returnSuccess1); err != nil {
		return nil, err
	}

	count, ok := b.memory.ReadUint32Le(countOut)
	if !ok {
		return nil, fmt.Errorf("dgw codec: frame count at %d outside module memory", countOut)
	}
	if count == 0 {
		return nil, nil
	}
	if count > 4096 {
		return nil, fmt.Errorf("dgw codec: decode reported %d frames", count)
	}
	framesPtr, ok := b.memory.ReadUint32Le(framesOut)
	if !ok {
		return nil, fmt.Errorf("dgw codec: frame array at %d outside module memory", framesOut)
	}
	frames := make([]dgw.Frame, 0, count)
	for i := uint32(0); i < count; i++ {
		f, err := b.readFrame(ctx, framesPtr, i)
		if err != nil {
			return nil, fmt.Errorf("dgw codec: frame %d: %w", i, err)
		}
		frames = append(frames, f)
	}
	return frames, nil
}

// readFrame reads one frame out of the array __getDGWFramePtr indexes.
func (b *dgwBridge) readFrame(ctx context.Context, framesPtr, index uint32) (dgw.Frame, error) {
	framePtr, err := callU32(ctx, b.frameAt, symbolFrameAt, uint64(framesPtr), uint64(index))
	if err != nil {
		return dgw.Frame{}, err
	}
	rawType, err := callU32(ctx, b.frameType, symbolFrameType, uint64(framePtr))
	if err != nil {
		return dgw.Frame{}, err
	}
	f := dgw.Frame{Direction: dgw.DirectionInbound, Type: dgw.FrameType(rawType)}
	if f.Type != 0 && !f.Type.Known() {
		return f, fmt.Errorf("unmapped frame type %d", rawType)
	}

	streamGroup := f.Type == dgw.FrameStreamGroupData ||
		f.Type == dgw.FrameStreamGroupEstabStream ||
		f.Type == dgw.FrameStreamGroupAck ||
		f.Type == dgw.FrameStreamGroupEndOfData
	if streamGroup {
		id, err := callU32(ctx, b.streamID, symbolStreamID, uint64(framePtr))
		if err != nil {
			return f, err
		}
		f.StreamID = dgw.StreamID(id)
	}

	switch f.Type {
	case dgw.FrameDrain:
		reason, err := callU32(ctx, b.drainReason, symbolDrainReason, uint64(framePtr))
		if err != nil {
			return f, err
		}
		f.DrainReason = reason
	case dgw.FrameStreamGroupEstabStream:
		data, err := b.readFrameBuffer(ctx, framePtr, b.estabParams, b.estabParamsLen, symbolEstabParams)
		if err != nil {
			return f, err
		}
		f.StreamData = data
	case dgw.FrameStreamGroupData:
		requiresAck, err := callU32(ctx, b.requiresAck, symbolRequiresAck, uint64(framePtr))
		if err != nil {
			return f, err
		}
		if requiresAck != 0 {
			f.RequiresAck = true
			ackID, err := callU32(ctx, b.ackID, symbolAckID, uint64(framePtr))
			if err != nil {
				return f, err
			}
			f.AckID = ackID
		}
		data, err := b.readFrameBuffer(ctx, framePtr, b.dataBytes, b.dataLen, symbolDataBytes)
		if err != nil {
			return f, err
		}
		f.StreamData = data
	}
	return f, nil
}

// readFrameBuffer reads a (pointer, length) pair from a frame and frees the
// module's copy; the module allocates it, the caller owns it from then on.
func (b *dgwBridge) readFrameBuffer(ctx context.Context, framePtr uint32, ptrFn, lenFn api.Function, name string) ([]byte, error) {
	ptr, err := callU32(ctx, ptrFn, name, uint64(framePtr))
	if err != nil {
		return nil, err
	}
	n, err := callU32(ctx, lenFn, name+" size", uint64(framePtr))
	if err != nil {
		return nil, err
	}
	defer b.freeBuffer(ctx, ptr)
	return b.readOut(ptr, n)
}

// EncodePing returns the encoded Ping frame.
func (b *dgwBridge) EncodePing() ([]byte, error) {
	return b.EncodeFrame(context.Background(), dgw.Frame{Direction: dgw.DirectionOutbound, Type: dgw.FramePing})
}

// EncodePong returns the encoded Pong frame. The module's Pong takes no argument
// from the received ping (the site caches one Pong and reuses it), so ping is
// checked but not otherwise used.
func (b *dgwBridge) EncodePong(ping dgw.Frame) ([]byte, error) {
	if ping.Type != 0 && ping.Type != dgw.FramePing {
		return nil, fmt.Errorf("dgw codec: EncodePong given %v, want Ping", ping.Type)
	}
	return b.EncodeFrame(context.Background(), dgw.Frame{Direction: dgw.DirectionOutbound, Type: dgw.FramePong})
}

// exported encoder names on the module that the site's loader exposes.
const (
	symbolFrameAt        = "__getDGWFramePtr"
	symbolFrameType      = "__getFrameType"
	symbolDrainReason    = "__getDrainReasonFromDrainFrame"
	symbolStreamID       = "__getStreamIdFromStreamGroupFrame"
	symbolEstabParams    = "__getEncodedParamsFromEstablishStreamFrame"
	symbolEstabParamsLen = "__getEncodedParamsSizeFromEstablishStreamFrame"
	symbolDataBytes      = "__getDataFromGroupedStreamDataFrame"
	symbolDataLen        = "__getDataSizeFromGroupedStreamDataFrame"
	symbolRequiresAck    = "__getRequiresAckFromGroupedStreamDataFrame"
	symbolAckID          = "__getAckIdFromGroupedStreamDataFrame"
)
