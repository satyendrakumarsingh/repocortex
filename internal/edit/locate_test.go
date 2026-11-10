package edit

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLocate(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		line    int
		cols    []int
		want    []int
		wantErr error
	}{
		// The measured tree-sitter case: the column is the call expression, not
		// the name. One occurrence wins regardless.
		{name: "single occurrence ignores a misleading column", src: "a\nx := s.repo.Save(id)\n", line: 2, cols: []int{5}, want: []int{14}},
		{name: "definition site with no column", src: "func (r *Repo) Save() {}\n", line: 1, cols: []int{-1}, want: []int{15}},
		{name: "substring of a longer name is not a match", src: "SaveAll()\n", line: 1, wantErr: ErrNotFound},
		{name: "dollar continues a JS identifier", src: "$Save()\n", line: 1, wantErr: ErrNotFound},
		{name: "two occurrences, column names one", src: "a.Save(); b.Save()\n", line: 1, cols: []int{12}, want: []int{12}},
		{name: "two occurrences, both named", src: "a.Save(); b.Save()\n", line: 1, cols: []int{12, 2, 12}, want: []int{2, 12}},
		{name: "two occurrences, column on neither", src: "a.Save(); b.Save()\n", line: 1, cols: []int{10}, wantErr: ErrAmbiguous},
		{name: "two occurrences, no column", src: "a.Save(); b.Save()\n", line: 1, wantErr: ErrAmbiguous},
		{name: "two occurrences, one site without a column", src: "a.Save(); b.Save()\n", line: 1, cols: []int{2, -1}, wantErr: ErrAmbiguous},
		{name: "CRLF line", src: "x\r\nr.Save()\r\n", line: 2, want: []int{5}},
		{name: "non-ASCII before the name", src: "é := Save()\n", line: 1, want: []int{6}},
		{name: "line past the end", src: "r.Save()\n", line: 3, wantErr: ErrNotFound},
		{name: "line zero", src: "r.Save()\n", line: 0, wantErr: ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Locate([]byte(tc.src), tc.line, "Save", tc.cols)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Locate: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSplice(t *testing.T) {
	src := []byte("a.Save(); b.Save()\n")
	got, err := Splice(src, "Save", "Store", []int{2, 12})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a.Store(); b.Store()\n" {
		t.Errorf("got %q", got)
	}
	if string(src) != "a.Save(); b.Save()\n" {
		t.Error("Splice modified its input")
	}

	for _, tc := range []struct {
		name    string
		offsets []int
		wantErr string
	}{
		{name: "overlap", offsets: []int{2, 2}, wantErr: "overlap"},
		{name: "bytes are not the old name", offsets: []int{0}, wantErr: "are not"},
		{name: "out of range", offsets: []int{100}, wantErr: "out of range"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Splice(src, "Save", "Store", tc.offsets); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

// FuzzLocate: Locate reads repository bytes it did not write. Whatever it is
// given, it must not panic, and every offset it returns must spell the name.
func FuzzLocate(f *testing.F) {
	f.Add("a.Save(); b.Save()\n", 1, "Save", 12)
	f.Add("é := Save()\r\n", 1, "Save", -1)
	f.Add("", 0, "", 0)
	f.Add("f(\"éééééé\", Save, Save)\n", 1, "Save", 18)
	f.Add("x := cafe\u0301\n", 1, "cafe", 5)
	f.Fuzz(func(t *testing.T, src string, line int, old string, col int) {
		got, err := Locate([]byte(src), line, old, []int{col})
		sgot, serr := NewSource([]byte(src)).Locate(line, old, []int{col})
		if (err == nil) != (serr == nil) || fmt.Sprint(got) != fmt.Sprint(sgot) {
			t.Fatalf("Source.Locate = %v, %v; Locate = %v, %v", sgot, serr, got, err)
		}
		if err != nil {
			return
		}
		if _, err := Splice([]byte(src), old, old+"x", got); err != nil {
			t.Fatalf("Splice refused Locate's own offsets %v: %v", got, err)
		}
		for _, at := range got {
			if at < 0 || at+len(old) > len(src) || src[at:at+len(old)] != old {
				t.Fatalf("Locate returned %d, which does not spell %q in %q", at, old, src)
			}
		}
	})
}

func BenchmarkLocate(b *testing.B) {
	src := []byte(strings.Repeat("// filler line that mentions nothing\n", 2000) + "\tr.Save(ctx, id)\n")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Locate(src, 2001, "Save", []int{1}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestLocateDistrustsAColumnTheTwoReadingsDisagreeOn: SCIP indexers for the
// JVM and TypeScript record UTF-16 code units, tree-sitter records bytes. On
// f("éééééé", Save, Save) the UTF-16 column of the SECOND Save (18) is the
// byte column of the FIRST. A column is trusted only where both readings
// select the same occurrence.
func TestLocateDistrustsAColumnTheTwoReadingsDisagreeOn(t *testing.T) {
	line := `f("éééééé", Save, Save)` + "\n"
	first := strings.Index(line, "Save")
	second := strings.LastIndex(line, "Save")
	utf16Second := len([]rune(line[:second])) // all BMP: runes == UTF-16 units

	for _, tc := range []struct {
		name string
		cols []int
	}{
		{name: "UTF-16 column of the second, byte column of the first", cols: []int{utf16Second}},
		{name: "byte column of the second, which no UTF-16 reading confirms", cols: []int{second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Locate([]byte(line), 1, "Save", tc.cols)
			if !errors.Is(err, ErrAmbiguous) {
				t.Fatalf("got %v, %v; want ErrAmbiguous (first Save is at %d)", got, err, first)
			}
		})
	}

	// With an ASCII prefix the two readings coincide and the column is used.
	got, err := Locate([]byte("a.Save(); b.Save() // é\n"), 1, "Save", []int{12})
	if err != nil || len(got) != 1 || got[0] != 12 {
		t.Fatalf("ASCII prefix: got %v, %v; want [12]", got, err)
	}

	// A supplementary-plane rune is two UTF-16 units and four bytes.
	src := "s(\"😀\", Save, Save)\n"
	b2 := strings.LastIndex(src, "Save")
	if got, err := Locate([]byte(src), 1, "Save", []int{b2}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("astral prefix: got %v, %v; want ErrAmbiguous", got, err)
	}
}

// TestLocateTreatsCombiningMarksAsPartOfTheIdentifier: in "café" spelled with
// a combining acute accent, "cafe" is a prefix of a longer identifier, not a
// match — renaming it would turn café into baŕ.
func TestLocateTreatsCombiningMarksAsPartOfTheIdentifier(t *testing.T) {
	for _, tc := range []struct {
		name, src string
	}{
		{name: "combining acute (Mn)", src: "x := café + 1\n"},
		{name: "spacing mark (Mc)", src: "x := cafeः + 1\n"},
		{name: "connector punctuation (Pc)", src: "x := cafe‿ + 1\n"},
		{name: "letter number (Nl)", src: "x := cafeⅠ + 1\n"},
		{name: "zero-width non-joiner", src: "x := cafe\u200c + 1\n"},
		{name: "zero-width joiner", src: "x := cafe\u200d + 1\n"},
		{name: "mark before the name", src: "x := ́cafe + 1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Locate([]byte(tc.src), 1, "cafe", nil); !errors.Is(err, ErrNotFound) {
				t.Fatalf("got %v, %v; want ErrNotFound", got, err)
			}
		})
	}
	if got, err := Locate([]byte("x := cafe + café\n"), 1, "cafe", nil); err != nil || len(got) != 1 || got[0] != 5 {
		t.Fatalf("plain cafe beside café: got %v, %v; want [5]", got, err)
	}
}

// TestSourceLocateAgreesWithLocate: the indexed form used for whole files
// returns what the one-shot form returns, line for line.
func TestSourceLocateAgreesWithLocate(t *testing.T) {
	src := []byte("a\r\nx := s.repo.Save(id)\n\na.Save(); b.Save()\nlast Save")
	s := NewSource(src)
	for line := 0; line <= 7; line++ {
		for _, cols := range [][]int{nil, {12}, {-1}} {
			want, werr := Locate(src, line, "Save", cols)
			got, gerr := s.Locate(line, "Save", cols)
			if (werr == nil) != (gerr == nil) || fmt.Sprint(want) != fmt.Sprint(got) {
				t.Fatalf("line %d cols %v: Source %v %v, Locate %v %v", line, cols, got, gerr, want, werr)
			}
		}
	}
}

// largeSource is n lines that each hold one site, about 60 bytes a line.
func largeSource(n int) ([]byte, map[int][]int) {
	var b strings.Builder
	lines := make(map[int][]int, n)
	for i := 1; i <= n; i++ {
		b.WriteString("\tr.Save(ctx, id) // padding padding padding padding padding\n")
		lines[i] = []int{-1}
	}
	return []byte(b.String()), lines
}

// TestLocateAndSpliceAreLinear: a 2.4 MB file with 40,000 sites. Locating
// each line from the top of the file and splicing back-to-front were both
// quadratic, which took tens of seconds here.
func TestLocateAndSpliceAreLinear(t *testing.T) {
	src, _ := largeSource(40000)
	start := time.Now()
	s := NewSource(src)
	offsets := make([]int, 0, 40000)
	for ln := 1; ln <= 40000; ln++ {
		at, err := s.Locate(ln, "Save", nil)
		if err != nil {
			t.Fatal(err)
		}
		offsets = append(offsets, at...)
	}
	out, err := Splice(src, "Save", "Store", offsets)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("locate+splice of %d bytes, %d sites took %v; want well under 1s", len(src), len(offsets), took)
	}
	if len(out) != len(src)+len(offsets) || bytes.Count(out, []byte("Store(")) != 40000 {
		t.Fatalf("splice output wrong: %d bytes", len(out))
	}
}

func BenchmarkLocateAndSplice1MiB2000Sites(b *testing.B) {
	filler := strings.Repeat("// filler line that mentions nothing at all ......\n", (1<<20)/52)
	base := strings.Count(filler, "\n")
	src := []byte(filler + strings.Repeat("\tr.Save(ctx, id)\n", 2000))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s := NewSource(src)
		offsets := make([]int, 0, 2000)
		for ln := base + 1; ln <= base+2000; ln++ {
			at, err := s.Locate(ln, "Save", nil)
			if err != nil {
				b.Fatal(err)
			}
			offsets = append(offsets, at...)
		}
		if _, err := Splice(src, "Save", "Store", offsets); err != nil {
			b.Fatal(err)
		}
	}
}

func TestOccurrencesIsWholeIdentifier(t *testing.T) {
	for _, tc := range []struct {
		line, name string
		want       int
	}{
		{"a.Save(); b.Save()", "Save", 2},
		{"SaveAll(); preSave(); Save_x", "Save", 0},
		{"// Save it", "Save", 1},
		{"Save", "", 0},
	} {
		if got := len(Occurrences([]byte(tc.line), tc.name)); got != tc.want {
			t.Errorf("Occurrences(%q, %q) = %d, want %d", tc.line, tc.name, got, tc.want)
		}
	}
}
