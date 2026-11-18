package pipeline

import (
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// TestCResolution indexes a small C project (headers in include/ and the .c files
// that define them, a namesake under third_party/, and five translation units)
// and checks every call against what `#include` makes it reach.
//
// What it is built to catch: `scale` and `clamp_int`, which third_party/other.c
// defines too and which only the file that includes util.h means, whether the
// header sits beside the includer (src/main.c), above it (`../include/util.h`
// from src/report.c) or on the include path (`<util.h>` from app/run.c); a
// `static` helper (`bump`) of util.c that stats.c, which does not include it,
// also calls by name, and which no other file can link to; and the C library
// (`printf`, `malloc`, `strlen`), which is the system's and not the repository's.
//
// What it does not bind, by design: a function reached through a header that an
// included header includes (only the file's own includes are read). A neighbour's
// function that no include of the file names binds at low and is listed as known:
// it is `bump`, which only the same-directory tier reaches.
func TestCResolution(t *testing.T) {
	got := langEdges(t, cFixture())
	high := model.ConfidenceHigh
	wants := []want{
		{"src/main.c:main", "src/main.c:local_total", high},
		{"src/main.c:main", "src/util.c:scale", high},
		{"src/main.c:main", "src/queue.c:queue_new", high},
		{"src/main.c:main", "src/queue.c:queue_push", high},
		{"src/main.c:main", "src/queue.c:queue_pop", high},
		{"src/main.c:main", "src/queue.c:queue_len", high},
		{"src/main.c:main", "src/queue.c:queue_free", high},
		{"src/queue.c:grow", "src/util.c:scale", high},
		{"src/queue.c:queue_new", "src/util.c:clamp_int", high},
		{"src/queue.c:queue_push", "src/queue.c:queue_len", high},
		{"src/queue.c:queue_push", "src/queue.c:grow", high},
		{"src/queue.c:queue_pop", "src/queue.c:queue_len", high},
		{"src/util.c:scale", "src/util.c:bump", high},
		{"src/util.c:scale", "src/util.c:clamp_int", high},
		{"src/report.c:report", "src/util.c:clamp_int", high},
		{"src/report.c:report_all", "src/report.c:report", high},
		{"src/report.c:report_all", "src/util.c:scale", high},
		{"app/run.c:run", "src/util.c:scale", high},
		{"app/run.c:run_twice", "app/run.c:run", high},
		{"third_party/other.c:scale", "third_party/other.c:clamp_int", high},
	}
	forbid := []want{
		{Src: "src/main.c:main", Dst: "third_party/other.c:scale"},
		{Src: "app/run.c:run", Dst: "third_party/other.c:scale"},
		{Src: "src/report.c:report", Dst: "third_party/other.c:clamp_int"},
		{Src: "third_party/other.c:scale", Dst: "src/util.c:clamp_int"},
		{Src: "src/main.c:main", Dst: "third_party/other.c:queue_len"},
		{Src: "src/queue.c:queue_pop", Dst: "third_party/other.c:queue_len"},
	}
	// A static function of a neighbour that the file does not include: no include
	// reaches it, so the same-directory tier binds it, wrongly and at low.
	known := want{Src: "src/stats.c:variance", Dst: "src/util.c:bump"}
	scoreEdges(t, got, wants, forbid, nil, known)
	capped(t, got, []want{{known.Src, known.Dst, model.ConfidenceLow}})
}

func cFixture() map[string]string {
	return map[string]string{
		"include/util.h": `#ifndef UTIL_H
#define UTIL_H

typedef struct { int total; } Stats;

int scale(int v, int k);
int clamp_int(int v, int lo, int hi);

#endif
`,
		"include/queue.h": `#ifndef QUEUE_H
#define QUEUE_H

typedef struct Queue Queue;

Queue *queue_new(int cap);
int queue_push(Queue *q, int v);
int queue_pop(Queue *q, int *out);
int queue_len(const Queue *q);
void queue_free(Queue *q);

#endif
`,
		"src/queue.c": `#include <stdlib.h>
#include <string.h>
#include "queue.h"
#include "util.h"

struct Queue {
  int *items;
  int head, tail, cap;
};

static int grow(Queue *q) {
  int cap = scale(q->cap, 2);
  int *bigger = realloc(q->items, cap * sizeof(int));
  if (!bigger) return -1;
  q->items = bigger;
  q->cap = cap;
  return 0;
}

Queue *queue_new(int cap) {
  Queue *q = calloc(1, sizeof(Queue));
  q->cap = clamp_int(cap, 1, 4096);
  q->items = malloc(q->cap * sizeof(int));
  return q;
}

int queue_len(const Queue *q) { return q->tail - q->head; }

int queue_push(Queue *q, int v) {
  if (queue_len(q) == q->cap && grow(q) != 0) return -1;
  q->items[q->tail++] = v;
  return 0;
}

int queue_pop(Queue *q, int *out) {
  if (queue_len(q) == 0) return -1;
  memcpy(out, &q->items[q->head++], sizeof(int));
  return 0;
}

void queue_free(Queue *q) {
  free(q->items);
  free(q);
}
`,
		"src/util.c": `#include "util.h"

static int bump(int v) { return v + 1; }

int clamp_int(int v, int lo, int hi) {
  if (v < lo) return lo;
  return v > hi ? hi : v;
}

int scale(int v, int k) { return clamp_int(bump(v) * k, 0, 1000); }
`,
		"third_party/other.h": `int scale(int v, int k);
int clamp_int(int v, int lo, int hi);
`,
		"third_party/other.c": `#include "other.h"

int clamp_int(int v, int lo, int hi) { return v; }

int queue_len(const void *q) { return 0; }

int scale(int v, int k) { return clamp_int(v * k, 0, 9); }
`,
		"src/main.c": `#include <stdio.h>
#include <stdlib.h>
#include "util.h"
#include "queue.h"

static int local_total(const int *v, int n) {
  int s = 0;
  for (int i = 0; i < n; ++i) s += v[i];
  return s;
}

int main(void) {
  int *buf = malloc(4 * sizeof(int));
  buf[0] = scale(2, 3);
  Queue *q = queue_new(8);
  queue_push(q, buf[0]);
  int top = 0;
  queue_pop(q, &top);
  printf("%d %d %d\n", local_total(buf, 4), queue_len(q), top);
  queue_free(q);
  free(buf);
  return 0;
}
`,
		"src/report.c": `#include <string.h>
#include "../include/util.h"

int report(const char *name, int v) {
  return clamp_int(v, 0, (int)strlen(name));
}

int report_all(const char *name, int v) { return report(name, scale(v, 2)); }
`,
		"app/run.c": `#include <util.h>

int run(int v) { return scale(v, 2); }

int run_twice(int v) { return run(run(v)); }
`,
		"src/stats.c": `int variance(const int *v, int n) { return bump(n); }
int helper_only_here(int x) { return x; }
`,
	}
}
