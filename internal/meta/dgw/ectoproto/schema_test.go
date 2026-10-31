package ectoproto_test

import (
	"testing"

	"github.com/me/metaai2api/internal/meta/dgw/ectoproto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// The generated types are hand-driven: protoc is not available, so the code was
// produced from the FileDescriptorProto shipped in the client bundle. These
// tests pin the field numbers the decoder depends on, because a wrong number
// decodes silently into the wrong field rather than failing.

func TestResponseFieldNumbers(t *testing.T) {
	// message Response: piece=1 is_first_chunk=2 is_final_chunk=3
	// response_id=4 async_metadata=9 unique_message_id=11 is_rollback=13
	m := (&ectoproto.Response{}).ProtoReflect()
	cases := map[string]int32{
		"piece":                1,
		"is_first_chunk":       2,
		"is_final_chunk":       3,
		"response_id":          4,
		"prompt_identifier":    5,
		"agent_trace_json":     7,
		"is_final_model_chunk": 8,
		"async_metadata":       9,
		"unique_message_id":    11,
		"is_rollback":          13,
	}
	fields := m.Descriptor().Fields()
	for name, want := range cases {
		fd := fields.ByName(protoreflect.Name(name))
		if fd == nil {
			t.Errorf("Response has no field %q", name)
			continue
		}
		if int32(fd.Number()) != want {
			t.Errorf("Response.%s number = %d, want %d", name, fd.Number(), want)
		}
	}
}

func TestResponsePieceOneofNumbers(t *testing.T) {
	// message ResponsePiece (oneof piece): model_response=4, reasoning_response=5,
	// static_response=1, progress_indicator=3, error=10, user_message_ack=13,
	// async_ack=16, deltas=15
	md := (&ectoproto.ResponsePiece{}).ProtoReflect().Descriptor()
	cases := map[string]int32{
		"static_response":    1,
		"progress_indicator": 3,
		"model_response":     4,
		"reasoning_response": 5,
		"error":              10,
		"user_message_ack":   13,
		"deltas":             15,
		"async_ack":          16,
	}
	fields := md.Fields()
	for name, want := range cases {
		fd := fields.ByName(protoreflect.Name(name))
		if fd == nil {
			t.Errorf("ResponsePiece has no field %q", name)
			continue
		}
		if int32(fd.Number()) != want {
			t.Errorf("ResponsePiece.%s number = %d, want %d", name, fd.Number(), want)
		}
	}
}

// TestModelResponseFields pins raw_chunk against pp_response_so_far: the decoder
// appends raw_chunk, and swapping them would duplicate the answer.
func TestModelResponseFields(t *testing.T) {
	md := (&ectoproto.ModelResponse{}).ProtoReflect().Descriptor()
	for name, want := range map[string]int32{
		"raw_chunk":          1,
		"pp_response_so_far": 2,
		"is_first_chunk":     4,
		"is_final_chunk":     5,
	} {
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Errorf("ModelResponse has no field %q", name)
			continue
		}
		if int32(fd.Number()) != want {
			t.Errorf("ModelResponse.%s number = %d, want %d", name, fd.Number(), want)
		}
	}
}

func TestErrorCodeValues(t *testing.T) {
	for name, want := range map[string]int32{
		"ERROR_CODE_UNKNOWN":              0,
		"ERROR_CODE_LOGGED_OUT_BLOCKED":   1,
		"ERROR_CODE_GLOBAL_RATE_LIMITED":  2,
		"ERROR_CODE_PROMPT_GUARD_BLOCKED": 3,
	} {
		v := ectoproto.ErrorCode_value[name]
		if v != want {
			t.Errorf("%s = %d, want %d", name, v, want)
		}
	}
}
