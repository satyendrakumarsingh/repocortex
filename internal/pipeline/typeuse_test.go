package pipeline

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// A class that is only held and passed, never constructed or called, had no
// reference at all. Its field, parameter and type-argument uses are references
// now, bound through the import that names the class, never to the
// constructor indexed under the same name; a library type is not counted as
// an unbound reference.
func TestJavaTypeUsesAreReferences(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/main/java/com/shop/repo/OrderRepo.java": `package com.shop.repo;

public class OrderRepo {
    public OrderRepo(String url) {}
}
`,
		"src/main/java/com/shop/service/OrderService.java": `package com.shop.service;

import com.shop.repo.OrderRepo;
import java.util.List;

public class OrderService {
    private final OrderRepo repo;
    private final String name;

    public OrderService(OrderRepo repo) {
        this.repo = repo;
        this.name = "orders";
    }

    public List<OrderRepo> all() {
        return null;
    }
}
`,
	})
	reg, runID, _ := indexTree(t, root, filepath.Join(t.TempDir(), "index.db"))

	cands, _, err := reg.SymbolCandidatesIn(ctx, runID, "OrderRepo", "repo/OrderRepo.java", 5)
	if err != nil {
		t.Fatal(err)
	}
	var class, ctor int64
	for _, c := range cands {
		switch c.Kind {
		case string(model.KindClass):
			class = c.ID
		case string(model.KindMethod):
			ctor = c.ID
		}
	}
	if class == 0 || ctor == 0 {
		t.Fatalf("fixture: class %d constructor %d", class, ctor)
	}
	edges, err := reg.Edges(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	refs := 0
	for _, e := range edges {
		if e.Kind != model.EdgeReferences {
			continue
		}
		if e.DstID == ctor {
			t.Errorf("a type use bound to the constructor: %+v", e)
		}
		if e.DstID == class {
			refs++
			if e.Confidence != model.ConfidenceHigh {
				t.Errorf("a type use through the class's import is %s, want high: %+v", e.Confidence, e)
			}
		}
	}
	if refs != 3 { // the field, the parameter and List<OrderRepo>
		t.Errorf("%d references to OrderRepo, want 3 (field, parameter, type argument)", refs)
	}
	stats, err := reg.ResolutionStats(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Ignored == 0 {
		t.Errorf("String and List were not set aside as library types: %+v", stats)
	}
}
