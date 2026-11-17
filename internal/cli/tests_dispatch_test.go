package cli

import (
	"strings"
	"testing"
)

// dispatchFixture is a Java interface, its one implementation, and a test
// written against the interface: the test's call is bound to Store.save, and
// no edge leads from it to SqlStore.save.
var dispatchFixture = map[string]string{
	"src/main/java/com/shop/Store.java": `package com.shop;

public interface Store {
  void save(String order);
}
`,
	"src/main/java/com/shop/SqlStore.java": `package com.shop;

public class SqlStore implements Store {
  public void save(String order) {
    System.out.println(order);
  }
}
`,
	"src/test/java/com/shop/StoreContractTest.java": `package com.shop;

import org.junit.jupiter.api.Test;

class StoreContractTest {
  @Test
  void savesThroughTheInterface() {
    Store s = new SqlStore();
    s.save("o-1");
  }
}
`,
}

// A test that calls a method through the interface runs the implementation.
// It was invisible to tests and to untested_critical: only edges into the
// implementation were walked.
func TestTestsFindsATestThatCallsThroughTheInterface(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, dispatchFixture)
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve", "--accuracy", "fast"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	out, err := run(t, root, "tests", "SqlStore.save")
	if err != nil {
		t.Fatalf("tests: %v\n%s", err, out)
	}
	if !strings.Contains(out, "savesThroughTheInterface") || !strings.Contains(out, "interface_dispatch") {
		t.Fatalf("the interface test is not found for the implementation:\n%s", out)
	}
	if !strings.Contains(out, "1 through a method save implements") {
		t.Errorf("the footer does not say how the test was found:\n%s", out)
	}

	// A second implementation: the test may hold that one instead.
	writeFiles(t, root, map[string]string{"src/main/java/com/shop/MemStore.java": `package com.shop;

public class MemStore implements Store {
  public void save(String order) {}
}
`})
	for _, args := range [][]string{{"index"}, {"resolve", "--accuracy", "fast"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	out, err = run(t, root, "tests", "SqlStore.save")
	if err != nil {
		t.Fatalf("tests: %v\n%s", err, out)
	}
	if !strings.Contains(out, "interface_dispatch  medium") {
		t.Fatalf("with two implementations the dispatch is at most medium:\n%s", out)
	}
}

// --path Store.java chose SqlStore.java, which contains it as a substring.
func TestPathPrefersWholeFileNames(t *testing.T) {
	root := isolate(t)
	writeFiles(t, root, dispatchFixture)
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve", "--accuracy", "fast"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	out, err := run(t, root, "callers", "save", "--path", "Store.java")
	if err != nil {
		t.Fatalf("callers: %v\n%s", err, out)
	}
	if !strings.Contains(out, "save (method) at src/main/java/com/shop/Store.java:4") {
		t.Fatalf("--path Store.java did not choose Store.java:\n%s", out)
	}
}
