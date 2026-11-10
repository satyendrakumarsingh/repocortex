package registry

import (
	"fmt"
	"testing"
)

func TestCandidateCacheInternBounded(t *testing.T) {
	cache := &CandidateCache{
		budget:   DefaultCandidateBudget,
		perName:  DefaultPerNameLimit,
		maxPaths: 10,
		maxKinds: 5,
		paths:    make(map[string]string),
		kinds:    make(map[string]string),
	}

	// 1. Intern identical paths - should reuse and not grow
	for i := 0; i < 20; i++ {
		p := cache.internPath("shared/path.go")
		if p != "shared/path.go" {
			t.Errorf("expected shared/path.go, got %s", p)
		}
	}
	if len(cache.paths) != 1 {
		t.Fatalf("expected 1 path, got %d", len(cache.paths))
	}

	// 2. Add up to maxPaths
	for i := 1; i < 10; i++ {
		cache.internPath(fmt.Sprintf("file_%d.go", i))
	}
	if len(cache.paths) != 10 {
		t.Fatalf("expected 10 paths at limit, got %d", len(cache.paths))
	}

	// 3. Adding one more should reset and bound the map
	cache.internPath("overflow.go")
	if len(cache.paths) > 10 {
		t.Fatalf("expected paths to be bounded <= 10, got %d", len(cache.paths))
	}
	if len(cache.paths) != 1 {
		t.Fatalf("expected paths to reset to 1 entry on overflow, got %d", len(cache.paths))
	}

	// Same for kinds
	for i := 0; i < 5; i++ {
		cache.internKind(fmt.Sprintf("kind_%d", i))
	}
	if len(cache.kinds) != 5 {
		t.Fatalf("expected 5 kinds, got %d", len(cache.kinds))
	}
	cache.internKind("overflow_kind")
	if len(cache.kinds) > 5 {
		t.Fatalf("expected kinds bounded <= 5, got %d", len(cache.kinds))
	}
	if len(cache.kinds) != 1 {
		t.Fatalf("expected kinds to reset to 1 entry on overflow, got %d", len(cache.kinds))
	}
}

func TestCandidateCacheDefaultMaxInternPaths(t *testing.T) {
	cache := NewCandidateCache(nil, 1, 100, 10)

	// Add 5000 paths
	for i := 0; i < DefaultMaxInternPaths; i++ {
		cache.internPath(fmt.Sprintf("path/to/file_%d.go", i))
	}
	if stats := cache.Stats(); stats.Paths != DefaultMaxInternPaths {
		t.Fatalf("expected %d paths, got %d", DefaultMaxInternPaths, stats.Paths)
	}

	// 5001st path triggers reset
	cache.internPath("path/to/file_overflow.go")
	if stats := cache.Stats(); stats.Paths != 1 {
		t.Fatalf("expected 1 path after reset, got %d", stats.Paths)
	}
}

func TestCandidateCacheAdmitAndLRUBudget(t *testing.T) {
	cache := NewCandidateCache(nil, 1, 5, 2)

	// Admit 3 candidates for "foo"
	fooCands := []SymbolIndexEntry{
		{ID: 1, Name: "foo", FilePath: "a.go"},
		{ID: 2, Name: "foo", FilePath: "a.go"},
		{ID: 3, Name: "foo", FilePath: "a.go"},
	}
	cache.admit("foo", fooCands)
	if cache.rows != 3 {
		t.Fatalf("expected 3 rows, got %d", cache.rows)
	}

	// Admit 3 candidates for "bar" -> total 6 > budget 5 -> "foo" evicted
	barCands := []SymbolIndexEntry{
		{ID: 4, Name: "bar", FilePath: "b.go"},
		{ID: 5, Name: "bar", FilePath: "b.go"},
		{ID: 6, Name: "bar", FilePath: "b.go"},
	}
	cache.admit("bar", barCands)

	stats := cache.Stats()
	if stats.Evictions != 1 {
		t.Errorf("expected 1 eviction, got %d", stats.Evictions)
	}
	if cache.rows != 3 {
		t.Errorf("expected 3 rows remaining, got %d", cache.rows)
	}
	if _, ok := cache.entries["foo"]; ok {
		t.Errorf("expected foo to be evicted from cache")
	}
	if _, ok := cache.entries["bar"]; !ok {
		t.Errorf("expected bar to remain in cache")
	}
}
