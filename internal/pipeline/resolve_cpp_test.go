package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestCPPResolution indexes a small C++ project (headers in include/ with the
// `.cpp` that defines them, an `.inl` of templates, a namesake under third_party/,
// and five translation units) and checks every call against what `#include` makes
// it reach.
//
// What it is built to catch: `total_area` and `clamp`, which third_party/other.cpp
// defines too and which only the file that includes the header means (an include
// of shape.hpp covers shape.cpp, which is how the pair is told); an `.hh`
// header, and an `.inl`, which pair with their sources by their stem; a function
// in an anonymous namespace (`helper`) of shape.cpp that stats.cpp, which does not
// include it, also calls by name, and which no other file can link to; and the
// standard library (`std::sort`, `std::cout`), which is the system's and not the
// repository's.
//
// What it does not bind, by design: a method of a class that third_party/other.cpp
// also defines (`st.push(3)`, where both `Stack`s have a `push`), because the
// two definitions are in different files and nothing in the call chooses between
// them; a function reached through a header that an included header includes
// (only the file's own includes are read); and a neighbour's function that no
// include of the file names, which binds at low and is listed as known.
func TestCPPResolution(t *testing.T) {
	got := langEdges(t, cppFixture())
	high, med := model.ConfidenceHigh, model.ConfidenceMedium
	wants := []want{
		{"src/main.cpp:main", "src/main.cpp:local_sum", high},
		{"src/main.cpp:main", "src/shape.cpp:total_area", high},
		{"src/main.cpp:main", "src/format.cpp:describe", high},
		{"src/main.cpp:main", "src/stack.cpp:peek_or", high},
		{"src/main.cpp:main", "src/main.cpp:fill", high},
		{"src/main.cpp:main", "src/stack.cpp:Stack.size", med},
		// A method declared in the header and defined in the .cpp: the header's
		// declaration is no symbol of its own, so the definition is the one reached.
		{"src/main.cpp:main", "src/shape.cpp:Shape.area", med},
		{"src/main.cpp:main", "include/shape.hpp:Shape", med},
		{"src/shape.cpp:total_area", "src/shape.cpp:helper", high},
		{"src/shape.cpp:total_area", "src/shape.cpp:clamp", high},
		{"src/shape.cpp:Shape.area", "src/shape.cpp:clamp", high},
		{"src/stack.cpp:Stack.push", "src/shape.cpp:clamp", high},
		{"src/stack.cpp:Stack.empty", "src/stack.cpp:Stack.size", high},
		{"src/stack.cpp:Stack.pop", "src/stack.cpp:Stack.empty", high},
		{"src/stack.cpp:peek_or", "src/stack.cpp:Stack.empty", med},
		{"src/stack.cpp:peek_or", "src/stack.cpp:Stack.size", med},
		{"src/render.cpp:render", "src/shape.cpp:clamp", high},
		{"src/render.cpp:render", "include/math.inl:square", high},
		{"src/render.cpp:render_twice", "src/render.cpp:render", high},
		{"third_party/other.cpp:total_area", "third_party/other.cpp:clamp", high},
		{"third_party/other.cpp:Stack.push", "third_party/other.cpp:clamp", high},
	}
	forbid := []want{
		{Src: "src/main.cpp:main", Dst: "third_party/other.cpp:total_area"},
		{Src: "src/main.cpp:main", Dst: "third_party/other.cpp:Stack"},
		{Src: "src/render.cpp:render", Dst: "third_party/other.cpp:clamp"},
		{Src: "src/stack.cpp:Stack.push", Dst: "third_party/other.cpp:clamp"},
		{Src: "third_party/other.cpp:total_area", Dst: "src/shape.cpp:clamp"},
	}
	// A function of an anonymous namespace in a neighbour that the file does not
	// include: no include reaches it, so the same-directory tier binds it, wrongly
	// and at low.
	known := want{Src: "src/stats.cpp:variance", Dst: "src/shape.cpp:helper"}
	scoreEdges(t, got, wants, forbid, nil, known)
	capped(t, got, []want{{known.Src, known.Dst, model.ConfidenceLow}})
}

func cppFixture() map[string]string {
	return map[string]string{
		"include/shape.hpp": `#pragma once
#include <vector>

class Shape {
 public:
  explicit Shape(double side);
  double area() const;

 private:
  double side_;
};

double clamp(double v, double lo, double hi);
double total_area(const std::vector<double>& sides);
`,
		"include/stack.hpp": `#pragma once
#include <vector>

class Stack {
 public:
  void push(int v);
  int pop();
  int size() const;
  bool empty() const;

 private:
  std::vector<int> items_;
};

int peek_or(const Stack& s, int fallback);
`,
		"src/stack.cpp": `#include "stack.hpp"
#include "shape.hpp"

void Stack::push(int v) { items_.push_back(static_cast<int>(clamp(v, 0.0, 1e6))); }

int Stack::size() const { return static_cast<int>(items_.size()); }

bool Stack::empty() const { return size() == 0; }

int Stack::pop() {
  if (empty()) return 0;
  int v = items_.back();
  items_.pop_back();
  return v;
}

int peek_or(const Stack& s, int fallback) { return s.empty() ? fallback : s.size(); }
`,
		"include/format.hh": `#pragma once
#include <string>

std::string describe(double v);
`,
		"include/math.inl": `#pragma once

template <typename T>
inline T square(T v) { return v * v; }
`,
		"src/shape.cpp": `#include "shape.hpp"
#include <algorithm>

namespace {
double helper(double v) { return v + 1.0; }
}

Shape::Shape(double side) : side_(side) {}

double Shape::area() const { return clamp(side_ * side_, 0.0, 100.0); }

double clamp(double v, double lo, double hi) { return std::min(std::max(v, lo), hi); }

double total_area(const std::vector<double>& sides) {
  double s = 0;
  for (double v : sides) s += clamp(helper(v), 0.0, 100.0);
  return s;
}
`,
		"src/format.cpp": `#include "format.hh"

std::string describe(double v) { return std::to_string(v); }
`,
		"third_party/other.hpp": `double clamp(double v, double lo, double hi);
double total_area(const double* sides, int n);
`,
		"third_party/other.cpp": `#include "other.hpp"

class Stack {
 public:
  void push(int v);
  int pop();
};

void Stack::push(int v) { clamp(v, 0, 9); }

int Stack::pop() { return 0; }

double clamp(double v, double lo, double hi) { return v; }

double total_area(const double* sides, int n) { return clamp(sides[0] * n, 0, 9); }
`,
		"src/main.cpp": `#include <iostream>
#include <vector>
#include "shape.hpp"
#include "format.hh"
#include "stack.hpp"

static double local_sum(const std::vector<double>& v) {
  double s = 0;
  for (double x : v) s += x;
  return s;
}

static int fill(Stack& st) {
  st.push(3);
  st.push(4);
  return st.pop();
}

int main() {
  std::vector<double> v{1, 2, 3};
  std::sort(v.begin(), v.end());
  Shape sh(2.0);
  v.push_back(sh.area());
  Stack st;
  int top = fill(st);
  std::cout << st.size() << peek_or(st, top);
  std::cout << describe(total_area(v)) << local_sum(v) << std::endl;
  return 0;
}
`,
		"src/render.cpp": `#include "shape.hpp"
#include "math.inl"

double render(double v) { return clamp(square(v), 0.0, 1.0); }

double render_twice(double v) { return render(render(v)); }
`,
		"src/stats.cpp": `double variance(double n) { return helper(n); }
double helper_only_here(double x) { return x; }
`,
	}
}
