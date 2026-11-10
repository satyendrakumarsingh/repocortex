package diffgraph

import (
	"context"
	"fmt"
	"os"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Example_refundEndpoint shows the shape of the output for the change the
// package doc leads with: a route that now writes a table, a call the compiler
// stopped proving, and a helper that lost its last caller.
//
// It doubles as a determinism check the toolchain enforces for free: go test
// compares this output byte for byte, so any map iteration that reached an
// ordering decision would make it flake.
func Example_refundEndpoint() {
	src := newFakeSource()

	newRun(src, 1, 0, model.StatusReady, "v1.4.0", "exact").
		file("pkg/api.go", "ok").
		file("pkg/service.go", "ok").
		file("db/schema.sql", "ok").
		sym("pkg/api.go", "pkg/api.go:GET /refunds", "GET /refunds", model.KindEndpoint, 10).
		sym("pkg/api.go", "pkg/api.go:handleRefund", "handleRefund", model.KindFunction, 20).
		sym("pkg/service.go", "pkg/service.go:Refund", "Refund", model.KindMethod, 5).
		sym("pkg/service.go", "pkg/service.go:legacyRefund", "legacyRefund", model.KindMethod, 80).
		sym("db/schema.sql", "db/schema.sql:refunds", "refunds", model.KindTable, 1).
		edge("pkg/api.go:handleRefund", "pkg/service.go:Refund", model.EdgeCalls,
			model.ConfidenceExact, model.SourceCompiler, "pkg/api.go", 22).
		edge("pkg/api.go:handleRefund", "pkg/service.go:legacyRefund", model.EdgeCalls,
			model.ConfidenceExact, model.SourceCompiler, "pkg/api.go", 23).
		edge("pkg/service.go:Refund", "db/schema.sql:refunds", model.EdgeReads,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/service.go", 8).
		endpoint("GET /refunds", "pkg/api.go", 10, "handleRefund", "pkg/api.go").
		table("refunds", true, 1, 0)

	newRun(src, 2, 9_000, model.StatusReady, "v1.5.0", "exact").
		file("pkg/api.go", "ok").
		file("pkg/service.go", "ok").
		file("db/schema.sql", "ok").
		sym("pkg/api.go", "pkg/api.go:GET /refunds", "GET /refunds", model.KindEndpoint, 12).
		sym("pkg/api.go", "pkg/api.go:handleRefund", "handleRefund", model.KindFunction, 22).
		sym("pkg/service.go", "pkg/service.go:Refund", "Refund", model.KindMethod, 5).
		sym("pkg/service.go", "pkg/service.go:legacyRefund", "legacyRefund", model.KindMethod, 80).
		sym("db/schema.sql", "db/schema.sql:refunds", "refunds", model.KindTable, 1).
		edge("pkg/api.go:handleRefund", "pkg/service.go:Refund", model.EdgeCalls,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/api.go", 24).
		edge("pkg/service.go:Refund", "db/schema.sql:refunds", model.EdgeWrites,
			model.ConfidenceHigh, model.SourceSourceCode, "pkg/service.go", 9).
		endpoint("GET /refunds", "pkg/api.go", 12, "handleRefundV2", "pkg/api.go").
		table("refunds", true, 0, 1)

	d, err := New()
	if err != nil {
		fmt.Println(err)
		return
	}
	rep, err := d.Compare(context.Background(),
		Side{Source: src, RunID: 1}, Side{Source: src, RunID: 2})
	if err != nil {
		fmt.Println(err)
		return
	}
	if err := Render(os.Stdout, rep); err != nil {
		fmt.Println(err)
	}

	// Output:
	// diff v1.4.0..v1.5.0 (runs 1..2)
	//   base: status=ready accuracy=exact digest=-
	//   head: status=ready accuracy=exact digest=-
	//
	// SYMBOLS
	//   moved    pkg/api.go:GET /refunds (endpoint)  line 10 -> 12 (+2)
	//   moved    pkg/api.go:handleRefund (function)  line 20 -> 22 (+2)
	//   orphaned pkg/service.go:legacyRefund (method)  inbound 1 -> 0
	//
	// EDGES
	//   downgraded pkg/api.go:handleRefund (function) -[CALLS]-> pkg/service.go:Refund (method)  exact -> high
	//              was: exact/compiler at pkg/api.go:22:4
	//              now: high/source_code at pkg/api.go:24:4
	//   removed    pkg/api.go:handleRefund (function) -[CALLS]-> pkg/service.go:legacyRefund (method)  exact/compiler at pkg/api.go:23:4
	//   removed    pkg/service.go:Refund (method) -[READS]-> db/schema.sql:refunds (table)  high/source_code at pkg/service.go:8:4
	//   added      pkg/service.go:Refund (method) -[WRITES]-> db/schema.sql:refunds (table)  high/source_code at pkg/service.go:9:4
	//
	// ENDPOINTS
	//   handler  GET /refunds (pkg/api.go): pkg/api.go:handleRefund -> pkg/api.go:handleRefundV2
	//
	// TABLES
	//   changed  refunds  readers 1 -> 0 (-1), writers 0 -> 1 (+1)
	//
	// COULD NOT COMPARE
	//   nothing: both runs were complete and comparable
}
