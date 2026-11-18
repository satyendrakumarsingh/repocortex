package parse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// 10,000 nested calls took an Erlang worker 28 s inside the definition query
// (quadratic in depth, and with no clock). Past maxQueryDepth the query is not
// run: the file is recorded as partial, says why, and costs a fraction of a
// second.
func TestAnAbsurdlyNestedFileIsRecordedAsPartialQuickly(t *testing.T) {
	depth := 4 * maxQueryDepth
	src := "-module(deep).\nf() -> " + strings.Repeat("f(", depth) + strings.Repeat(")", depth) + ".\n"
	p := NewRegistry().ForLanguage("erlang")
	began := time.Now()
	res, err := p.Parse(context.Background(), "deep.erl", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("took %s", took)
	}
	if res.Status != StatusPartial || res.Reason != model.ParseReasonBudgetExceeded {
		t.Fatalf("status %s reason %s, want partial and budget_exceeded", res.Status, res.Reason)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "nests more than") {
		t.Errorf("the note does not say why: %v", res.Err)
	}
}

func TestOrdinaryNestingIsStillQueried(t *testing.T) {
	src := "-module(shallow).\nf() -> " + strings.Repeat("g(", 100) + strings.Repeat(")", 100) + ".\n"
	p := NewRegistry().ForLanguage("erlang")
	res, err := p.Parse(context.Background(), "shallow.erl", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Symbols) == 0 || res.Status == StatusPartial && res.Reason == model.ParseReasonBudgetExceeded {
		t.Errorf("a file nested 100 deep lost its definitions: %s %v symbols=%d", res.Status, res.Err, len(res.Symbols))
	}
}
