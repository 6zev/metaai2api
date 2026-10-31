// Command protogen turns a FileDescriptorProto into Go source, without protoc.
//
// protoc normally does this: it parses .proto, builds a CodeGeneratorRequest and
// hands it to a plugin. meta.ai ships only the compiled descriptor, so this
// command builds the same request by hand and writes the plugin's output.
//
// Usage: protogen <descriptor.bin> <output-dir> <go-package-name>
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: protogen <descriptor.bin>[:generate] [more.desc...] <out-dir> <go-package>")
		fmt.Fprintln(os.Stderr, "  a descriptor suffixed :generate is emitted; the rest are only resolved.")
		os.Exit(2)
	}
	outDir := os.Args[len(os.Args)-2]
	goPkg := os.Args[len(os.Args)-1]
	inputs := os.Args[1 : len(os.Args)-2]

	var (
		files    []*descriptorpb.FileDescriptorProto
		generate []string
	)
	for _, in := range inputs {
		wantGenerate := false
		path := in
		if i := strings.LastIndex(in, ":"); i > 0 && in[i+1:] == "generate" {
			path, wantGenerate = in[:i], true
		}
		raw, err := os.ReadFile(path)
		check(err)
		var fd descriptorpb.FileDescriptorProto
		check(proto.Unmarshal(raw, &fd))
		if fd.GetName() == "" {
			fd.Name = proto.String(filepath.Base(path))
		}
		files = append(files, &fd)
		if wantGenerate {
			generate = append(generate, fd.GetName())
		}
	}
	if len(generate) == 0 {
		fmt.Fprintln(os.Stderr, "no descriptor marked :generate")
		os.Exit(2)
	}

	// One Go package covers every file, so each gets the same go_package.
	for _, fd := range files {
		fd.Options = &descriptorpb.FileOptions{GoPackage: proto.String(goPkg)}
	}

	// A descriptor does not record its imports, but the plugin needs them to
	// resolve cross-file types. Add each other file as a dependency, ordered so
	// no cycle forms: a file may only depend on files earlier in the argument
	// list, and only if it actually references a type declared there.
	gen := map[string]bool{}
	for _, n := range generate {
		gen[n] = true
	}
	declared := map[string]string{} // fully-qualified type -> file
	for _, fd := range files {
		for _, m := range fd.GetMessageType() {
			declared[fd.GetPackage()+"."+m.GetName()] = fd.GetName()
		}
		for _, e := range fd.GetEnumType() {
			declared[fd.GetPackage()+"."+e.GetName()] = fd.GetName()
		}
	}
	for idx, fd := range files {
		if !gen[fd.GetName()] {
			continue
		}
		have := map[string]bool{}
		for _, d := range fd.GetDependency() {
			have[d] = true
		}
		for _, other := range files[:idx] {
			if other.GetName() == fd.GetName() || have[other.GetName()] {
				continue
			}
			// Does fd reference any type declared in other?
			if referencesTypeOf(fd, other, declared) {
				fd.Dependency = append(fd.Dependency, other.GetName())
			}
		}
	}

	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: generate,
		ProtoFile:      files,
		CompilerVersion: &pluginpb.Version{
			Major: proto.Int32(3), Minor: proto.Int32(21), Patch: proto.Int32(12),
		},
	}
	in, err := proto.Marshal(req)
	check(err)

	// Run the installed plugin.
	cmd := exec.Command("protoc-gen-go")
	cmd.Stdin = strings.NewReader(string(in))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "protoc-gen-go:", err)
		os.Exit(1)
	}

	var resp pluginpb.CodeGeneratorResponse
	check(proto.Unmarshal(out, &resp))
	if resp.Error != nil {
		fmt.Fprintln(os.Stderr, "plugin reported:", resp.GetError())
		os.Exit(1)
	}
	if len(resp.File) == 0 {
		fmt.Fprintln(os.Stderr, "plugin produced no files")
		os.Exit(1)
	}

	for _, f := range resp.File {
		path := filepath.Join(outDir, filepath.Base(f.GetName()))
		check(os.MkdirAll(filepath.Dir(path), 0o755))
		check(os.WriteFile(path, []byte(f.GetContent()), 0o644))
		fmt.Println("wrote", path, len(f.GetContent()), "bytes")
	}
}

// referencesTypeOf reports whether any message field in fd names a type that the
// given file declares.
func referencesTypeOf(fd, other *descriptorpb.FileDescriptorProto, declared map[string]string) bool {
	var walk func(msgs []*descriptorpb.DescriptorProto) bool
	walk = func(msgs []*descriptorpb.DescriptorProto) bool {
		for _, m := range msgs {
			for _, f := range m.GetField() {
				tn := strings.TrimPrefix(f.GetTypeName(), ".")
				if fn, ok := declared[tn]; ok && fn == other.GetName() {
					return true
				}
			}
			if walk(m.GetNestedType()) {
				return true
			}
		}
		return false
	}
	return walk(fd.GetMessageType())
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
