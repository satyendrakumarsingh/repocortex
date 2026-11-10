package registry

import (
	"context"
	"testing"
)

// The producer stamp exists because a stale index has no symptom.
//
// An index is data plus the resolution rules that produced it, and this project
// changed those rules twice in one working session — once breaking every chained
// Go call, once converting 362 ambiguous references into confident wrong
// answers. An index written either side of such a change holds different edges
// for identical source. Every query against it still succeeds and still carries
// confidence tiers, so nothing looks wrong. The index has to say so itself.

// TestARunRecordsTheBuildThatWroteIt is the basic contract.
func TestARunRecordsTheBuildThatWroteIt(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t)
	r.producerVersion = "1.4.2"

	run, err := r.BeginRun(ctx, "auto", "rev1")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	if run.ProducerVersion != "1.4.2" {
		t.Errorf("BeginRun returned producer %q, want %q", run.ProducerVersion, "1.4.2")
	}

	// And it must survive the round trip to disk, which is the only version that
	// matters: the stamp is read back by a DIFFERENT process than wrote it.
	got, err := r.Run(ctx, run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got.ProducerVersion != "1.4.2" {
		t.Errorf("stored producer = %q, want %q", got.ProducerVersion, "1.4.2")
	}
}

// TestAnUnstampedRunReportsUnknownRatherThanMatching is the case that decides
// whether this feature is honest.
//
// Every run written before the stamp existed has no version. Defaulting those to
// the running binary's version would assert something false about all of them and
// silently mark stale indexes as current — the exact failure the stamp exists to
// prevent, reintroduced by its own migration default. Empty must stay empty, and
// callers distinguish "unknown provenance" from "known mismatch" because the two
// warrant different advice.
func TestAnUnstampedRunReportsUnknownRatherThanMatching(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRegistry(t) // no producerVersion set — an old writer

	run, err := r.BeginRun(ctx, "auto", "rev1")
	if err != nil {
		t.Fatalf("begin run: %v", err)
	}
	got, err := r.Run(ctx, run.ID)
	if err != nil {
		t.Fatalf("read run: %v", err)
	}
	if got.ProducerVersion != "" {
		t.Errorf("an unstamped run reported producer %q; it must stay empty so "+
			"'unknown provenance' cannot be mistaken for 'matches this build'",
			got.ProducerVersion)
	}
}

// TestWithProducerVersionIsTheOnlyWayIn keeps the layering honest: the version
// lives in the CLI and is injected, because an adapter that reached upward for
// it would invert the dependency arrow (CLAUDE.md §1).
func TestWithProducerVersionIsTheOnlyWayIn(t *testing.T) {
	r, _ := newTestRegistry(t)
	if r.producerVersion != "" {
		t.Errorf("a registry built without the option claims producer %q", r.producerVersion)
	}
	WithProducerVersion("2.0.0")(r)
	if r.producerVersion != "2.0.0" {
		t.Errorf("WithProducerVersion did not apply: %q", r.producerVersion)
	}
}
