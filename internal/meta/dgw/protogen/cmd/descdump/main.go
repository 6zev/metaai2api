// Command descdump parses a protobuf FileDescriptorProto (the raw bytes of a
// .proto descriptor embedded in the meta.ai bundle) and prints the message and
// field table.
//
// It exists because the meta.ai capture ships descriptors, not .proto sources:
// the generated Go types cannot be produced without protoc, so the schema has to
// be read out of the descriptor at generation time and written into this
// package by hand.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: descdump <descriptor.bin>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var fd descriptorpb.FileDescriptorProto
	if err := proto.Unmarshal(raw, &fd); err != nil {
		fmt.Fprintln(os.Stderr, "unmarshal:", err)
		os.Exit(1)
	}
	fmt.Printf("package %s\nsyntax %s\n\n", fd.GetPackage(), fd.GetSyntax())
	for _, m := range fd.GetMessageType() {
		dumpMessage(m, fd.GetPackage())
	}
	for _, e := range fd.GetEnumType() {
		dumpEnum(e, fd.GetPackage())
	}
}

func dumpMessage(m *descriptorpb.DescriptorProto, pkg string) {
	name := pkg + "." + m.GetName()
	fmt.Printf("message %s {\n", name)
	fields := append([]*descriptorpb.FieldDescriptorProto{}, m.GetField()...)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].GetNumber() < fields[j].GetNumber()
	})
	for _, f := range fields {
		fmt.Printf("  %d %s %s %s\n",
			f.GetNumber(), cardinality(f), typeName(f), f.GetName())
		if f.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE ||
			f.GetType() == descriptorpb.FieldDescriptorProto_TYPE_ENUM {
			fmt.Printf("      option (%s) = true\n", strings.TrimPrefix(f.GetTypeName(), "."))
		}
	}
	for _, nm := range m.GetNestedType() {
		fmt.Println()
		dumpMessage(nm, name)
	}
	for _, e := range m.GetEnumType() {
		fmt.Println()
		dumpEnum(e, name)
	}
	fmt.Printf("}\n\n")
}

func dumpEnum(e *descriptorpb.EnumDescriptorProto, parent string) {
	fmt.Printf("enum %s.%s {\n", parent, e.GetName())
	for _, v := range e.GetValue() {
		fmt.Printf("  %s = %d\n", v.GetName(), v.GetNumber())
	}
	fmt.Printf("}\n\n")
}

func cardinality(f *descriptorpb.FieldDescriptorProto) string {
	switch f.GetLabel() {
	case descriptorpb.FieldDescriptorProto_LABEL_REPEATED:
		return "repeated"
	case descriptorpb.FieldDescriptorProto_LABEL_REQUIRED:
		return "required"
	default:
		return "optional"
	}
}

func typeName(f *descriptorpb.FieldDescriptorProto) string {
	switch f.GetType() {
	case descriptorpb.FieldDescriptorProto_TYPE_DOUBLE:
		return "double"
	case descriptorpb.FieldDescriptorProto_TYPE_FLOAT:
		return "float"
	case descriptorpb.FieldDescriptorProto_TYPE_INT64:
		return "int64"
	case descriptorpb.FieldDescriptorProto_TYPE_UINT64:
		return "uint64"
	case descriptorpb.FieldDescriptorProto_TYPE_INT32:
		return "int32"
	case descriptorpb.FieldDescriptorProto_TYPE_FIXED64:
		return "fixed64"
	case descriptorpb.FieldDescriptorProto_TYPE_FIXED32:
		return "fixed32"
	case descriptorpb.FieldDescriptorProto_TYPE_BOOL:
		return "bool"
	case descriptorpb.FieldDescriptorProto_TYPE_STRING:
		return "string"
	case descriptorpb.FieldDescriptorProto_TYPE_GROUP:
		return "group"
	case descriptorpb.FieldDescriptorProto_TYPE_MESSAGE:
		return "message"
	case descriptorpb.FieldDescriptorProto_TYPE_BYTES:
		return "bytes"
	case descriptorpb.FieldDescriptorProto_TYPE_UINT32:
		return "uint32"
	case descriptorpb.FieldDescriptorProto_TYPE_ENUM:
		return "enum"
	case descriptorpb.FieldDescriptorProto_TYPE_SFIXED32:
		return "sfixed32"
	case descriptorpb.FieldDescriptorProto_TYPE_SFIXED64:
		return "sfixed64"
	case descriptorpb.FieldDescriptorProto_TYPE_SINT32:
		return "sint32"
	case descriptorpb.FieldDescriptorProto_TYPE_SINT64:
		return "sint64"
	default:
		return "unknown"
	}
}
