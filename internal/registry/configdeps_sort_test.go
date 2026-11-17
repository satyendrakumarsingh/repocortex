package registry

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

// insertionSortConfigKeys is the algorithm sortConfigKeys replaced, kept to
// prove the replacement orders keys exactly as it did.
func insertionSortConfigKeys(list []ConfigKeyUsage) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0; j-- {
			a, b := list[j-1], list[j]
			if (!b.Declared && a.Declared) || (b.Declared == a.Declared && b.Name < a.Name) {
				list[j-1], list[j] = list[j], list[j-1]
				continue
			}
			break
		}
	}
}

func TestSortConfigKeysMatchesTheInsertionSortItReplaced(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 50; trial++ {
		n := rng.Intn(200)
		in := make([]ConfigKeyUsage, n)
		for i := range in {
			// Few distinct names, so equal keys exercise stability.
			in[i] = ConfigKeyUsage{Name: fmt.Sprintf("K%02d", rng.Intn(20)), Declared: rng.Intn(2) == 0, Readers: i}
		}
		want := append([]ConfigKeyUsage(nil), in...)
		got := append([]ConfigKeyUsage(nil), in...)
		insertionSortConfigKeys(want)
		sortConfigKeys(got)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("trial %d: orders differ", trial)
		}
	}
}
