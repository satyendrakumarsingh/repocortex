package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Transitive sourcing and includes: A sources (includes) B, B sources C, C sources
// D, and A calls a function that only C or D defines. The shell reads those files
// while it reads B, and the preprocessor pastes the headers B includes, so the
// call is in scope; before this only A's own files were read.
//
// Every fixture is built to catch the same four things:
//
//	depth    a function two and three files down binds (high: one definition on
//	         the chain)
//	closer   a name defined by a nearer file and again by a farther one binds the
//	         nearer, whichever file was indexed first
//	cycle    the last file sources the first again, which ends the walk and binds
//	         nothing wrongly
//	decoy    a file of the same stem under another directory, and one that no file
//	         on the chain names, define the same names and receive no edge, and a
//	         script or shader that sources nothing reaches none of them
//
// The decoys are what keep this from becoming name matching: a reached file is
// matched by its whole path, not by the tail a direct import is matched by.

func TestBashTransitiveSourcing(t *testing.T) {
	got := langEdges(t, map[string]string{
		"scripts/a.sh": `source ./lib/b.sh

run() {
  from_b
  from_c
  from_d
  shared
  deep
}
`,
		"scripts/lib/b.sh": `source ./c.sh

from_b() { :; }

shared() { echo "b"; }
`,
		"scripts/lib/c.sh": `. ./d.sh

from_c() { :; }

deep() { echo "c"; }
`,
		"scripts/lib/d.sh": `source ./b.sh

from_d() { :; }

shared() { echo "d"; }

deep() { echo "d"; }
`,
		// Same stem under another directory, and a file no one sources.
		"other/c.sh": `from_c() { :; }

deep() { echo "other"; }
`,
		"scripts/lib/unsourced.sh": `from_c() { :; }

from_d() { :; }

shared() { echo "unsourced"; }
`,
		// Sources nothing.
		"scripts/e.sh": `run_e() {
  from_c
  from_d
}
`,
		// Sources only c.sh, so d.sh is one step down and b.sh is not on its chain
		// except through d.sh's cycle.
		"scripts/f.sh": `source ./lib/c.sh

run_f() {
  from_c
  from_d
  from_b
}
`,
	})
	high := model.ConfidenceHigh
	wants := []want{
		{"scripts/a.sh:run", "scripts/lib/b.sh:from_b", high},
		{"scripts/a.sh:run", "scripts/lib/c.sh:from_c", high},
		{"scripts/a.sh:run", "scripts/lib/d.sh:from_d", high},
		// Defined by b (one step) and d (three): b's.
		{"scripts/a.sh:run", "scripts/lib/b.sh:shared", high},
		// Defined by c (two steps) and d (three), and by other/c.sh, which is not c.
		{"scripts/a.sh:run", "scripts/lib/c.sh:deep", high},
		{"scripts/f.sh:run_f", "scripts/lib/c.sh:from_c", high},
		{"scripts/f.sh:run_f", "scripts/lib/d.sh:from_d", high},
		// The cycle d -> b is followed once: b is reached from f, two steps down.
		{"scripts/f.sh:run_f", "scripts/lib/b.sh:from_b", high},
	}
	forbid := []want{
		{Src: "scripts/a.sh", Dst: "other/c.sh"},
		{Src: "scripts/a.sh", Dst: "scripts/lib/unsourced.sh"},
		{Src: "scripts/a.sh", Dst: "scripts/lib/d.sh:shared"},
		{Src: "scripts/a.sh", Dst: "scripts/lib/d.sh:deep"},
		{Src: "scripts/f.sh", Dst: "other/c.sh"},
		{Src: "scripts/f.sh", Dst: "scripts/lib/unsourced.sh"},
		{Src: "scripts/e.sh", Dst: "scripts/lib/"},
		{Src: "scripts/e.sh", Dst: "other/"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func TestPowerShellTransitiveDotSourcing(t *testing.T) {
	got := langEdges(t, map[string]string{
		"scripts/a.ps1": `. ./lib/b.ps1

function Invoke-A {
  Get-FromB
  Get-FromC
  Get-FromD
  Get-Shared
}
`,
		"scripts/lib/b.ps1": `. "$PSScriptRoot\c.ps1"

function Get-FromB { return 1 }

function Get-Shared { return "b" }
`,
		"scripts/lib/c.ps1": `. "$PSScriptRoot\d.ps1"

function Get-FromC { return 2 }
`,
		"scripts/lib/d.ps1": `. "$PSScriptRoot\b.ps1"

function Get-FromD { return 3 }

function Get-Shared { return "d" }
`,
		"other/c.ps1": `function Get-FromC { return 0 }
`,
		"scripts/lib/unsourced.ps1": `function Get-FromC { return 0 }

function Get-FromD { return 0 }
`,
		"scripts/e.ps1": `function Invoke-E {
  Get-FromC
  Get-FromD
}
`,
	})
	high := model.ConfidenceHigh
	wants := []want{
		{"scripts/a.ps1:Invoke-A", "scripts/lib/b.ps1:Get-FromB", high},
		{"scripts/a.ps1:Invoke-A", "scripts/lib/c.ps1:Get-FromC", high},
		{"scripts/a.ps1:Invoke-A", "scripts/lib/d.ps1:Get-FromD", high},
		{"scripts/a.ps1:Invoke-A", "scripts/lib/b.ps1:Get-Shared", high},
	}
	forbid := []want{
		{Src: "scripts/a.ps1", Dst: "other/c.ps1"},
		{Src: "scripts/a.ps1", Dst: "scripts/lib/unsourced.ps1"},
		{Src: "scripts/a.ps1", Dst: "scripts/lib/d.ps1:Get-Shared"},
		{Src: "scripts/e.ps1", Dst: "scripts/lib/"},
		{Src: "scripts/e.ps1", Dst: "other/"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func TestCTransitiveIncludes(t *testing.T) {
	got := langEdges(t, map[string]string{
		"src/a.c": `#include "../include/b.h"

int run(int v) {
  return from_b(v) + from_c(v) + from_d(v) + shared(v) + deep(v);
}
`,
		"include/b.h": `#include "c.h"

int from_b(int v);
int shared(int v);
`,
		"src/b.c": `#include "../include/b.h"

int from_b(int v) { return v; }
int shared(int v) { return 1; }
`,
		"include/c.h": `#include "d.h"

int from_c(int v);
int deep(int v);
`,
		"src/c.c": `#include "../include/c.h"

int from_c(int v) { return v; }
int deep(int v) { return 2; }
`,
		"include/d.h": `#include "b.h"

int from_d(int v);
int shared(int v);
int deep(int v);
`,
		"src/d.c": `#include "../include/d.h"

int from_d(int v) { return v; }
int shared(int v) { return 3; }
int deep(int v) { return 4; }
`,
		// A header of the same stem elsewhere, and a unit nothing includes.
		"third_party/c.h": `int from_c(int v);
`,
		"third_party/c.c": `#include "c.h"

int from_c(int v) { return 0; }
`,
		"lib/unincluded.c": `int from_c(int v) { return 0; }

int from_d(int v) { return 0; }
`,
		// Includes nothing of the chain.
		"src/e.c": `int run_e(int v) {
  return from_c(v) + from_d(v);
}
`,
	})
	high := model.ConfidenceHigh
	wants := []want{
		{"src/a.c:run", "src/b.c:from_b", high},
		{"src/a.c:run", "src/c.c:from_c", high},
		{"src/a.c:run", "src/d.c:from_d", high},
		// Declared by b.h (one step) and d.h (three): b's.
		{"src/a.c:run", "src/b.c:shared", high},
		// Declared by c.h (two steps) and d.h (three): c's.
		{"src/a.c:run", "src/c.c:deep", high},
	}
	forbid := []want{
		{Src: "src/a.c", Dst: "third_party/"},
		{Src: "src/a.c", Dst: "lib/unincluded.c"},
		{Src: "src/a.c", Dst: "src/d.c:shared"},
		{Src: "src/a.c", Dst: "src/d.c:deep"},
	}
	// The unit beside the chain that includes none of it: its calls reach the
	// definitions of the same directory at low, as they did before.
	known := []want{
		{Src: "src/e.c:run_e", Dst: "src/c.c:from_c"},
		{Src: "src/e.c:run_e", Dst: "src/d.c:from_d"},
	}
	scoreEdges(t, got, wants, forbid, nil, known...)
	capped(t, got, []want{
		{"src/e.c:run_e", "src/c.c:from_c", model.ConfidenceLow},
		{"src/e.c:run_e", "src/d.c:from_d", model.ConfidenceLow},
	})
}

func TestGLSLTransitiveIncludes(t *testing.T) {
	got := langEdges(t, map[string]string{
		"shaders/main.frag": `#version 450
#include "lib/lighting.glsl"

void main() {
  float a = base_light(0.5);
  float b = from_math(a);
  float c = from_util(b);
  float d = shared_fn(c);
}
`,
		"shaders/lib/lighting.glsl": `#include "math.glsl"

float base_light(float x) { return x; }

float shared_fn(float x) { return 1.0; }
`,
		"shaders/lib/math.glsl": `#include "util.glsl"
#include "lighting.glsl"

float from_math(float x) { return x; }
`,
		"shaders/lib/util.glsl": `float from_util(float x) { return x; }

float shared_fn(float x) { return 2.0; }
`,
		"shaders/legacy/util.glsl": `float from_util(float x) { return 0.0; }
`,
		"shaders/lib/unused.glsl": `float from_math(float x) { return 0.0; }

float from_util(float x) { return 0.0; }
`,
		"shaders/plain.frag": `#version 450
void main() {
  float a = from_math(0.5);
  float b = from_util(a);
}
`,
	})
	high := model.ConfidenceHigh
	wants := []want{
		{"shaders/main.frag:main", "shaders/lib/lighting.glsl:base_light", high},
		{"shaders/main.frag:main", "shaders/lib/math.glsl:from_math", high},
		{"shaders/main.frag:main", "shaders/lib/util.glsl:from_util", high},
		{"shaders/main.frag:main", "shaders/lib/lighting.glsl:shared_fn", high},
	}
	forbid := []want{
		{Src: "shaders/main.frag", Dst: "shaders/legacy/"},
		{Src: "shaders/main.frag", Dst: "shaders/lib/unused.glsl"},
		{Src: "shaders/main.frag", Dst: "shaders/lib/util.glsl:shared_fn"},
		{Src: "shaders/plain.frag", Dst: "shaders/"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func TestHLSLTransitiveIncludes(t *testing.T) {
	got := langEdges(t, map[string]string{
		"shaders/main.hlsl": `#include "lib/lighting.hlsli"

float4 main() : SV_Target {
  float a = base_light(0.5);
  float b = from_math(a);
  float c = from_util(b);
  return float4(c, c, c, 1);
}
`,
		"shaders/lib/lighting.hlsli": `#include "math.hlsli"

float base_light(float x) { return x; }
`,
		"shaders/lib/math.hlsli": `#include "util.hlsli"
#include "lighting.hlsli"

float from_math(float x) { return x; }
`,
		"shaders/lib/util.hlsli": `float from_util(float x) { return x; }
`,
		"shaders/legacy/util.hlsli": `float from_util(float x) { return 0; }
`,
		"shaders/plain.hlsl": `float4 plain() : SV_Target {
  float a = from_math(0.5);
  float b = from_util(a);
  return float4(b, b, b, 1);
}
`,
	})
	high := model.ConfidenceHigh
	wants := []want{
		{"shaders/main.hlsl:main", "shaders/lib/lighting.hlsli:base_light", high},
		{"shaders/main.hlsl:main", "shaders/lib/math.hlsli:from_math", high},
		{"shaders/main.hlsl:main", "shaders/lib/util.hlsli:from_util", high},
	}
	forbid := []want{
		{Src: "shaders/main.hlsl", Dst: "shaders/legacy/"},
		{Src: "shaders/plain.hlsl", Dst: "shaders/"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}
