package autorun

import (
	"errors"
	"testing"
	"time"
)

func TestARecordedPassReadsBack(t *testing.T) {
	dir := t.TempDir()
	if Read(dir) != nil {
		t.Fatal("a directory with no record read as a pass")
	}
	at := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	if err := Write(dir, at, 1500*time.Millisecond, nil); err != nil {
		t.Fatal(err)
	}
	if r := Read(dir); r == nil || !r.OK || r.DurationMS != 1500 || r.At != "2026-11-17T09:30:00Z" || r.Error != "" {
		t.Fatalf("ok pass read as %+v", r)
	}
	if err := Write(dir, at, time.Second, errors.New("resolve: database is locked\nretry")); err != nil {
		t.Fatal(err)
	}
	if r := Read(dir); r == nil || r.OK || r.Error != "resolve: database is locked retry" {
		t.Fatalf("failed pass read as %+v", r)
	}
}
