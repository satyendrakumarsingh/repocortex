package parse

import (
	"path"
	"strings"
)

// CUDA, GLSL and HLSL: `#include "x.cuh"`, `#include "common.glsl"`, `#include
// "lighting.hlsli"`. The grammars read the directive as C's grammar does, and the
// resolver compares an import with a file by the file's stem (a `.cu` and the
// `.cuh` it pairs with are one name, as a `.m` and `.h` are in Objective-C), so
// the extension is dropped and the include is rewritten to the files it can name.
//
// CUDA is C++ and keeps the rule of the C family: a quoted include is looked for
// beside the including file and then on the include path, which the source does
// not state. Both readings are recorded (see objcImport, which this follows).
//
// GLSL has no #include in the core language. GL_GOOGLE_include_directive and
// GL_ARB_shading_language_include add it, and glslang, shaderc and every engine's
// shader preprocessor read it the same way, so it is the form real shader trees
// use. HLSL's preprocessor reads it natively.
func init() {
	registerImportFix("cuda", includeFileImport)
	registerImportFix("glsl", includeFileImport)
	registerImportFix("hlsl", includeFileImport)
}

// includeFileImport names the file an include reaches, without its extension.
// An angle-bracket include (`<cuda_runtime.h>`, `<thrust/sort.h>`) is looked for
// on the include path only, and is kept as written: a header of the repository
// that sits on that path is the one it names, and a system header names nothing
// the resolver can find.
func includeFileImport(filePath string, imp Import) []Import {
	p := strings.TrimSpace(imp.Path)
	angled := strings.HasPrefix(p, "<")
	p = path.Clean(strings.Trim(p, "<>"))
	if ext := path.Ext(p); ext != "" {
		p = p[:len(p)-len(ext)]
	}
	if p == "" || p == "." || p[0] == '/' {
		return nil
	}
	var out []Import
	if !strings.HasPrefix(p, "..") {
		written := imp
		written.Path = p
		out = append(out, written)
	}
	if angled {
		return out
	}
	if dir := path.Dir(filePath); dir != "." {
		rooted := path.Join(dir, p)
		if !strings.HasPrefix(rooted, "..") {
			row := imp
			row.Path = rooted
			out = append(out, row)
		}
	} else if strings.HasPrefix(p, "..") {
		return nil
	}
	return out
}
