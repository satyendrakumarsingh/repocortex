package missed

import (
	"context"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

func TestDetectorEmptyRunNegative(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	det := NewDetector(reg, capability.NewDeps(reg, nil))
	rep, err := det.Detect(ctx, run.ID, DefaultOptions())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}

	if rep.TotalBlindspots != 0 {
		t.Errorf("expected 0 blindspots on empty run, got %d", rep.TotalBlindspots)
	}
}

func TestDetectorCleanRunNegative(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}

	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Clean file with ok parse status
	f := model.File{Path: "src/clean.go", Language: "go", ContentHash: "hashClean", ParseStatus: "ok"}
	fileID, err := w.AddFile(ctx, &f)
	if err != nil {
		t.Fatal(err)
	}

	// Clean declared configuration
	cfgSym := model.Symbol{
		FileID:     fileID,
		Kind:       model.KindConfiguration,
		Name:       "PORT",
		FQN:        "config.PORT",
		Confidence: model.ConfidenceHigh,
		Source:     model.SourceConfig,
		Exported:   true,
	}
	if _, err := w.AddSymbol(ctx, &cfgSym); err != nil {
		t.Fatal(err)
	}

	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	det := NewDetector(reg, capability.NewDeps(reg, nil))
	rep, err := det.Detect(ctx, run.ID, DefaultOptions())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}

	if rep.TotalBlindspots != 0 {
		t.Errorf("expected 0 blindspots on clean run, got %d: %+v", rep.TotalBlindspots, rep.Blindspots)
	}
}

func TestDetectorFindsBlindspotsPositive(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "fast", "")
	if err != nil {
		t.Fatal(err)
	}

	w, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Add file with partial parse status
	f := model.File{
		Path: "src/broken.go", Language: "go", ContentHash: "hash1", ParseStatus: model.ParseStatusPartial,
		ParseError: model.FormatParseError(model.ParseReasonBudgetExceeded,
			"parser stopped at byte 10 of 900 (line 2) on its iteration limit (30 of 30); definitions after that point are missing"),
	}
	fileID, err := w.AddFile(ctx, &f)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Add an unresolved reference
	ref := registry.Reference{
		FileID: fileID,
		Name:   "FetchExternal",
		Kind:   "call",
		Line:   10,
	}
	if err := w.AddReference(ctx, fileID, ref); err != nil {
		t.Fatal(err)
	}

	// 3. Add an undeclared config usage
	cfgUsage := registry.ConfigUsage{
		Key:       "SECRET_API_KEY",
		Mechanism: "os.Getenv",
		Line:      12,
	}
	if err := w.AddConfigUsage(ctx, fileID, cfgUsage); err != nil {
		t.Fatal(err)
	}

	// 4. Add an orphan table access (read 0, write 0)
	tblAccess := registry.TableAccess{
		Table:   "legacy_tokens",
		Access:  "read",
		Line:    15,
		Certain: true,
	}
	if err := w.AddTableAccess(ctx, fileID, tblAccess); err != nil {
		t.Fatal(err)
	}

	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// Mark reference as unresolved in DB
	_, err = db.ExecContext(ctx, `UPDATE symbol_references SET resolution = 'unresolved' WHERE index_run_id = ?`, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	det := NewDetector(reg, capability.NewDeps(reg, nil))
	rep, err := det.Detect(ctx, run.ID, DefaultOptions())
	if err != nil {
		t.Fatalf("detect: %v", err)
	}

	if rep.TotalBlindspots == 0 {
		t.Fatal("expected blindspots to be detected, got 0")
	}
	if len(rep.Capped) != 0 {
		t.Errorf("Capped = %v with the default limit and one finding per kind; want none", rep.Capped)
	}

	// At a limit of one, every kind that found something stopped there, and
	// its count is a lower bound the report has to say is one.
	tight, err := det.Detect(ctx, run.ID, Options{MaxItemsPerKind: 1})
	if err != nil {
		t.Fatalf("detect with limit 1: %v", err)
	}
	if len(tight.Capped) != len(tight.ByKind) || len(tight.Capped) == 0 {
		t.Errorf("Capped = %v, ByKind = %v; want every kind with a finding marked capped", tight.Capped, tight.ByKind)
	}

	var foundUnparsed, foundConfig, foundUnresolved bool
	for _, b := range rep.Blindspots {
		if b.Kind == BlindspotUnparsedFile && b.Target == "src/broken.go" {
			foundUnparsed = true
			if b.Severity != "high" {
				t.Errorf("expected high severity for unparsed file, got %s", b.Severity)
			}
		}
		if b.Kind == BlindspotMissingConfigDecl && b.Target == "SECRET_API_KEY" {
			foundConfig = true
		}
		if b.Kind == BlindspotUnresolvedCall && b.Target == "FetchExternal" {
			foundUnresolved = true
		}
	}

	if !foundUnparsed {
		t.Error("expected BlindspotUnparsedFile for src/broken.go")
	}
	if !foundConfig {
		t.Error("expected BlindspotMissingConfigDecl for SECRET_API_KEY")
	}
	if !foundUnresolved {
		t.Error("expected BlindspotUnresolvedCall for FetchExternal")
	}
}
