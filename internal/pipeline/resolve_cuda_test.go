package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestCUDAResolution indexes a small CUDA project (a header and the `.cu` that
// defines it, a vendored namesake, and three translation units) and checks every
// call against what `#include` makes it reach.
//
// What it is built to catch: `scale_kernel` and `dot3`, which third_party/other.cu
// declares too and which only the file that includes vec.cuh means (an include of
// the header covers the `.cu` that pairs with it); a kernel launch
// `scale_kernel<<<g, b>>>(...)`, which is a call of the kernel; `clamp01`, a device
// function that both vec.cu and other.cu define; a `static` helper of main.cu that
// stats.cu, which does not include it, also calls by name; and the runtime
// (`cudaMalloc`, `cudaMemcpy`, `__syncthreads`, `atomicAdd`, `printf`), which is
// the toolkit's and not the repository's.
//
// What it does not bind, by design: a function reached through a header that an
// included header includes (only the file's own includes are read), a call on an
// object of a class (CUDA's definitions carry no class), and a neighbour's function
// that no include of the file names.
func TestCUDAResolution(t *testing.T) {
	got := langEdges(t, cudaFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"src/main.cu:main", "src/main.cu:local_sum", high},
		{"src/main.cu:main", "src/vec.cu:launch_scale", high},
		{"src/main.cu:main", "src/vec.cu:dot3", high},
		// The kernel launch in another file that includes the header.
		{"src/reduce.cu:reduce", "src/vec.cu:scale_kernel", high},
		// Within vec.cu, and within the vendored copy.
		{"src/vec.cu:launch_scale", "src/vec.cu:scale_kernel", high},
		{"src/vec.cu:scale_kernel", "src/vec.cu:clamp01", high},
		{"third_party/other.cu:scale_kernel", "third_party/other.cu:clamp01", high},
	}
	forbid := []want{
		{Src: "src/", Dst: "third_party/"},
		{Src: "src/stats.cu", Dst: "src/main.cu"},
		{Src: "src/vec.cu:scale_kernel", Dst: "third_party/other.cu:clamp01"},
		{Src: "third_party/", Dst: "src/"},
	}
	scoreEdges(t, got, wants, forbid, nil)
}

func cudaFixture() map[string]string {
	return map[string]string{
		"include/vec.cuh": `#pragma once
struct Vec3 { float x, y, z; };
__host__ __device__ float dot3(const Vec3& a, const Vec3& b);
__global__ void scale_kernel(float* data, float k, int n);
void launch_scale(float* data, float k, int n);
`,
		"src/vec.cu": `#include "vec.cuh"

__host__ __device__ float dot3(const Vec3& a, const Vec3& b) {
  return a.x * b.x + a.y * b.y + a.z * b.z;
}

__device__ float clamp01(float v) { return fminf(fmaxf(v, 0.0f), 1.0f); }

__global__ void scale_kernel(float* data, float k, int n) {
  int i = blockIdx.x * blockDim.x + threadIdx.x;
  if (i < n) {
    data[i] = clamp01(data[i] * k);
  }
  __syncthreads();
}

void launch_scale(float* data, float k, int n) {
  float* d = nullptr;
  cudaMalloc(&d, n * sizeof(float));
  cudaMemcpy(d, data, n * sizeof(float), cudaMemcpyHostToDevice);
  scale_kernel<<<(n + 255) / 256, 256>>>(d, k, n);
  cudaDeviceSynchronize();
  cudaFree(d);
}
`,
		"third_party/other.cuh": `__global__ void scale_kernel(float* data, float k, int n);
float dot3(float a, float b);
`,
		"third_party/other.cu": `#include "other.cuh"

__device__ float clamp01(float v) { return v; }

__global__ void scale_kernel(float* data, float k, int n) {
  data[0] = clamp01(k);
}

float dot3(float a, float b) { return a * b; }
`,
		"src/main.cu": `#include <cstdio>
#include <cuda_runtime.h>
#include "vec.cuh"

static float local_sum(const float* v, int n) {
  float s = 0;
  for (int i = 0; i < n; ++i) s += v[i];
  return s;
}

int main() {
  float h[4] = {1, 2, 3, 4};
  launch_scale(h, 2.0f, 4);
  Vec3 a{1, 2, 3}, b{4, 5, 6};
  printf("%f %f\n", dot3(a, b), local_sum(h, 4));
  cudaError_t err = cudaGetLastError();
  return err == cudaSuccess ? 0 : 1;
}
`,
		"src/reduce.cu": `#include "vec.cuh"

__global__ void reduce(float* data, int n) {
  atomicAdd(&data[0], data[1]);
  scale_kernel<<<1, 32>>>(data, 0.5f, n);
}
`,
		"src/stats.cu": `#include "vec.cuh"

float variance(const float* v, int n) { return local_sum(v, n); }
float helper_only_here(float x) { return x; }
`,
	}
}
