package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestGLSLResolution indexes a small set of shaders (a math library and a lighting
// library that includes it, a legacy copy of the math library, and three fragment
// shaders) and checks every call against what `#include` makes it reach.
//
// What it is built to catch: `saturate`, `gamma` and `luminance`, which common/
// math.glsl and legacy/math.glsl both define, where only the file a shader includes
// is meant; a shader that includes nothing and calls `saturate` (no definition is in
// its translation unit, so none is bound); a `luminance` of a shader's own, which is
// not the library's; and the built-ins (`texture`, `normalize`, `dot`, `clamp`,
// `pow`, `vec3`), which are the language's.
//
// A function in a file that an included file includes binds as well
// (TestGLSLTransitiveIncludes). What it does not bind, by design: the entry point
// `main`, which no shader calls.
func TestGLSLResolution(t *testing.T) {
	got := langEdges(t, glslFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"shaders/common/lighting.glsl:diffuse", "shaders/common/math.glsl:saturate", high},
		{"shaders/common/lighting.glsl:tonemap", "shaders/common/math.glsl:gamma", high},
		{"shaders/pbr.frag:fresnel", "shaders/common/math.glsl:saturate", high},
		{"shaders/pbr.frag:main", "shaders/common/lighting.glsl:diffuse", high},
		{"shaders/pbr.frag:main", "shaders/common/lighting.glsl:tonemap", high},
		{"shaders/pbr.frag:main", "shaders/common/math.glsl:luminance", high},
		{"shaders/pbr.frag:main", "shaders/pbr.frag:fresnel", high},
		{"shaders/post.frag:main", "shaders/legacy/math.glsl:gamma", high},
		{"shaders/post.frag:main", "shaders/legacy/math.glsl:luminance", high},
		{"shaders/sky.frag:main", "shaders/sky.frag:luminance", high},
	}
	forbid := []want{
		{Src: "shaders/pbr.frag", Dst: "shaders/legacy/"},
		{Src: "shaders/common/", Dst: "shaders/legacy/"},
		{Src: "shaders/post.frag", Dst: "shaders/common/"},
		// No include: nothing of math.glsl is in this shader.
		{Src: "shaders/sky.frag", Dst: "math.glsl"},
		{Src: "shaders/pbr.frag:main", Dst: "shaders/sky.frag"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func glslFixture() map[string]string {
	return map[string]string{
		"shaders/common/math.glsl": `#define PI 3.14159

float saturate(float x) { return clamp(x, 0.0, 1.0); }

vec3 gamma(vec3 c) { return pow(c, vec3(1.0 / 2.2)); }

float luminance(vec3 c) { return dot(c, vec3(0.2126, 0.7152, 0.0722)); }
`,
		"shaders/common/lighting.glsl": `#include "math.glsl"

struct Light { vec3 dir; vec3 color; };

vec3 diffuse(vec3 n, Light l) {
  float d = saturate(dot(n, -l.dir));
  return l.color * d;
}

vec3 tonemap(vec3 c) { return gamma(c / (c + vec3(1.0))); }
`,
		"shaders/legacy/math.glsl": `float saturate(float x) { return min(max(x, 0.0), 1.0); }
vec3 gamma(vec3 c) { return c; }
float luminance(vec3 c) { return c.r; }
`,
		"shaders/pbr.frag": `#version 450
#include "common/lighting.glsl"
#include "common/math.glsl"

layout(location = 0) in vec3 vNormal;
layout(location = 0) out vec4 outColor;
uniform sampler2D albedo;

float fresnel(float c) { return pow(1.0 - saturate(c), 5.0); }

void main() {
  Light l;
  vec3 base = texture(albedo, vec2(0.5)).rgb;
  vec3 lit = diffuse(normalize(vNormal), l) * base;
  float f = fresnel(luminance(lit));
  outColor = vec4(tonemap(lit) * f, 1.0);
}
`,
		"shaders/sky.frag": `#version 450
layout(location = 0) out vec4 outColor;

float luminance(vec3 c) { return c.g; }

void main() {
  float x = saturate(0.5);
  outColor = vec4(vec3(luminance(vec3(x))), 1.0);
}
`,
		"shaders/post.frag": `#version 450
#include "legacy/math.glsl"
layout(location = 0) out vec4 outColor;
void main() {
  outColor = vec4(gamma(vec3(luminance(vec3(0.3)))), 1.0);
}
`,
	}
}
