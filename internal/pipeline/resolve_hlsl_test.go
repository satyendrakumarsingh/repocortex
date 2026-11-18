package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestHLSLResolution indexes a small set of shaders (a math library and a lighting
// library that includes it, a legacy copy of the math library, and three entry
// points) and checks every call against what `#include` makes it reach.
//
// What it is built to catch: `gamma_correct` and `luma`, which common/math.hlsli and
// legacy/math.hlsli both define, where only the file a shader includes is meant; a
// shader that includes nothing and calls `gamma_correct` (nothing of the library is
// in its translation unit); a `luma` of a shader's own; the intrinsics (`saturate`,
// `dot`, `pow`, `normalize`, `float3`), which are the language's; and a method of a
// built-in resource (`albedo.Sample(samp, uv)`), which no file defines.
//
// What it does not bind, by design: `luma` in pbr.hlsl, which reaches math.hlsli
// only through the include of lighting.hlsli (only a file's own includes are read),
// and a method called on a value.
func TestHLSLResolution(t *testing.T) {
	got := langEdges(t, hlslFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"shaders/common/lighting.hlsli:tonemap", "shaders/common/math.hlsli:gamma_correct", high},
		{"shaders/pbr.hlsl:main", "shaders/common/lighting.hlsli:diffuse", high},
		{"shaders/pbr.hlsl:main", "shaders/common/lighting.hlsli:tonemap", high},
		{"shaders/pbr.hlsl:main", "shaders/pbr.hlsl:fresnel", high},
		{"shaders/post.hlsl:main", "shaders/legacy/math.hlsli:gamma_correct", high},
		{"shaders/post.hlsl:main", "shaders/legacy/math.hlsli:luma", high},
		{"shaders/sky.hlsl:main", "shaders/sky.hlsl:luma", high},
	}
	forbid := []want{
		{Src: "shaders/pbr.hlsl", Dst: "shaders/legacy/"},
		{Src: "shaders/post.hlsl", Dst: "shaders/common/"},
		{Src: "shaders/sky.hlsl", Dst: "math.hlsli"},
		{Src: "shaders/pbr.hlsl:main", Dst: "shaders/sky.hlsl"},
		{Src: "shaders/", Dst: ":Sample"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func hlslFixture() map[string]string {
	return map[string]string{
		"shaders/common/math.hlsli": `float3 gamma_correct(float3 c) { return pow(c, 1.0 / 2.2); }

float luma(float3 c) { return dot(c, float3(0.2126, 0.7152, 0.0722)); }
`,
		"shaders/common/lighting.hlsli": `#include "math.hlsli"

struct Light { float3 dir; float3 color; };

float3 diffuse(float3 n, Light l) {
  float d = saturate(dot(n, -l.dir));
  return l.color * d;
}

float3 tonemap(float3 c) { return gamma_correct(c / (c + 1.0)); }
`,
		"shaders/legacy/math.hlsli": `float3 gamma_correct(float3 c) { return c; }
float luma(float3 c) { return c.r; }
`,
		"shaders/pbr.hlsl": `#include "common/lighting.hlsli"

Texture2D albedo : register(t0);
SamplerState samp : register(s0);

float fresnel(float c) { return pow(1.0 - saturate(c), 5.0); }

float4 main(float3 n : NORMAL, float2 uv : TEXCOORD0) : SV_Target {
  Light l;
  float3 base = albedo.Sample(samp, uv).rgb;
  float3 lit = diffuse(normalize(n), l) * base;
  float f = fresnel(luma(lit));
  return float4(tonemap(lit) * f, 1.0);
}
`,
		"shaders/sky.hlsl": `float luma(float3 c) { return c.g; }

float4 main(float2 uv : TEXCOORD0) : SV_Target {
  float x = gamma_correct(float3(uv, 0.0)).x;
  return float4(luma(float3(x, x, x)), 0, 0, 1);
}
`,
		"shaders/post.hlsl": `#include "legacy/math.hlsli"

float4 main(float2 uv : TEXCOORD0) : SV_Target {
  return float4(gamma_correct(float3(luma(float3(uv, 0.0)), 0, 0)), 1.0);
}
`,
	}
}
