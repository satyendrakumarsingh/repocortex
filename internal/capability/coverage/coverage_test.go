package coverage

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestParseGoCover(t *testing.T) {
	data := `mode: set
github.com/example/pkg/service.go:10.2,15.10 2 1
github.com/example/pkg/service.go:16.2,20.10 1 0
github.com/example/pkg/handler.go:5.2,8.15 3 5
`
	rep, err := ParseGoCover(strings.NewReader(data))
	if err != nil {
		t.Fatalf("ParseGoCover: %v", err)
	}

	if rep.Format != "go-cover" {
		t.Errorf("format = %q, want go-cover", rep.Format)
	}
	if len(rep.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(rep.Files))
	}

	cov, covLines, totLines := rep.IsSymbolCovered("github.com/example/pkg/service.go", 10, 15)
	if !cov || covLines == 0 {
		t.Errorf("expected lines 10-15 to be covered, got covered=%v (%d/%d)", cov, covLines, totLines)
	}

	covUncovered, _, _ := rep.IsSymbolCovered("github.com/example/pkg/service.go", 16, 20)
	if covUncovered {
		t.Errorf("expected lines 16-20 to be uncovered")
	}
}

func TestParseLCOV(t *testing.T) {
	data := `TN:
SF:src/index.js
DA:1,1
DA:2,1
DA:3,0
DA:4,2
end_of_record
`
	rep, err := ParseLCOV(strings.NewReader(data))
	if err != nil {
		t.Fatalf("ParseLCOV: %v", err)
	}

	if rep.Format != "lcov" {
		t.Errorf("format = %q, want lcov", rep.Format)
	}
	fc, ok := rep.Files["src/index.js"]
	if !ok {
		t.Fatalf("file src/index.js not found in report")
	}
	if fc.TotalLines != 4 || fc.CoveredLines != 3 {
		t.Errorf("expected 3/4 lines, got %d/%d", fc.CoveredLines, fc.TotalLines)
	}
}

func TestParseCobertura(t *testing.T) {
	data := `<?xml version="1.0" ?>
<coverage version="5.5">
  <packages>
    <package name="pkg">
      <classes>
        <class name="calc.py" filename="pkg/calc.py">
          <lines>
            <line number="1" hits="1"/>
            <line number="2" hits="0"/>
            <line number="3" hits="1"/>
          </lines>
        </class>
      </classes>
    </package>
  </packages>
</coverage>
`
	rep, err := ParseCobertura(strings.NewReader(data))
	if err != nil {
		t.Fatalf("ParseCobertura: %v", err)
	}

	if rep.Format != "cobertura" {
		t.Errorf("format = %q, want cobertura", rep.Format)
	}
	fc, ok := rep.Files["pkg/calc.py"]
	if !ok {
		t.Fatalf("file pkg/calc.py not found in report")
	}
	if fc.TotalLines != 3 || fc.CoveredLines != 2 {
		t.Errorf("expected 2/3 lines, got %d/%d", fc.CoveredLines, fc.TotalLines)
	}
}

const jacocoReport = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<report name="demo">
  <package name="com/acme/x">
    <class name="com/acme/x/Foo" sourcefilename="Foo.java"/>
    <sourcefile name="Foo.java">
      <line nr="10" mi="0" ci="3" mb="0" cb="0"/>
      <line nr="11" mi="2" ci="0" mb="0" cb="0"/>
      <line nr="12" mi="1" ci="4" mb="1" cb="1"/>
      <line nr="13" mi="0" ci="0" mb="0" cb="0"/>
    </sourcefile>
  </package>
</report>
`

func TestParseJaCoCo(t *testing.T) {
	rep, err := ParseJaCoCo(strings.NewReader(jacocoReport))
	if err != nil {
		t.Fatalf("ParseJaCoCo: %v", err)
	}
	if rep.Format != "jacoco" {
		t.Errorf("format = %q, want jacoco", rep.Format)
	}
	fc, ok := rep.Files["com/acme/x/Foo.java"]
	if !ok {
		t.Fatalf("file com/acme/x/Foo.java not found, got %v", rep.Files)
	}
	// Line 13 has no instructions and is skipped; 11 is uncovered; 10 and the
	// partially covered 12 count as covered.
	if fc.TotalLines != 3 || fc.CoveredLines != 2 {
		t.Errorf("expected 2/3 lines, got %d/%d", fc.CoveredLines, fc.TotalLines)
	}
	if _, present := fc.Lines[13]; present {
		t.Errorf("line with no instructions must be skipped")
	}
	if fc.Lines[11] != 0 || fc.Lines[12] == 0 {
		t.Errorf("line 11 should be uncovered and line 12 covered, got %v", fc.Lines)
	}
}

func TestJaCoCoPathMapsToSourceRoot(t *testing.T) {
	rep, err := ParseJaCoCo(strings.NewReader(jacocoReport))
	if err != nil {
		t.Fatalf("ParseJaCoCo: %v", err)
	}
	for _, p := range []string{
		"src/main/java/com/acme/x/Foo.java",
		"module/src/main/kotlin/com/acme/x/Foo.java",
	} {
		cov, covered, total := rep.IsSymbolCovered(p, 10, 12)
		if !cov || covered != 2 || total != 3 {
			t.Errorf("%s: covered=%v %d/%d, want true 2/3", p, cov, covered, total)
		}
	}
}

func TestParseJaCoCoDoctype(t *testing.T) {
	data := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<!DOCTYPE report PUBLIC "-//JACOCO//DTD Report 1.1//EN" "report.dtd">
` + strings.TrimPrefix(jacocoReport, `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+"\n")
	rep, err := parseXMLReport(strings.NewReader(data))
	if err != nil {
		t.Fatalf("parseXMLReport with DOCTYPE: %v", err)
	}
	if rep.Format != "jacoco" || rep.CoveredLines != 2 {
		t.Errorf("format=%q covered=%d, want jacoco/2", rep.Format, rep.CoveredLines)
	}
}

func TestParseXMLReportDetection(t *testing.T) {
	cob := `<?xml version="1.0"?><coverage><packages><package name="p"><classes>
<class filename="p/a.py"><lines><line number="1" hits="1"/></lines></class></classes></package></packages></coverage>`
	rep, err := parseXMLReport(strings.NewReader(cob))
	if err != nil || rep.Format != "cobertura" {
		t.Fatalf("cobertura detection: format=%v err=%v", rep, err)
	}
	rep, err = parseXMLReport(strings.NewReader(jacocoReport))
	if err != nil || rep.Format != "jacoco" {
		t.Fatalf("jacoco detection: format=%v err=%v", rep, err)
	}
	if _, err := parseXMLReport(strings.NewReader(`<?xml version="1.0"?><other/>`)); err == nil {
		t.Errorf("unknown root element should be an error")
	}
}

func TestParseCoverageFileJaCoCo(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"jacoco.xml", "jacoco-report"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(jacocoReport), 0o600); err != nil {
			t.Fatal(err)
		}
		rep, err := ParseCoverageFile(p)
		if err != nil || rep.Format != "jacoco" {
			t.Errorf("%s: format=%v err=%v", name, rep, err)
		}
	}
}

// Maven's report-aggregate nests packages in <group> elements, and a class in
// the default package has no directory.
func TestParseJaCoCoAggregateAndDefaultPackage(t *testing.T) {
	src := `<?xml version="1.0" encoding="UTF-8"?>
<report name="all">
  <group name="service-a">
    <group name="nested">
      <package name="com/acme/a">
        <sourcefile name="A.java"><line nr="3" mi="0" ci="2" mb="0" cb="0"/></sourcefile>
      </package>
    </group>
  </group>
  <package name="">
    <sourcefile name="Main.java"><line nr="1" mi="4" ci="0" mb="0" cb="0"/></sourcefile>
  </package>
</report>`
	rep, err := ParseJaCoCo(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for p := range rep.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if strings.Join(paths, ",") != "Main.java,com/acme/a/A.java" {
		t.Fatalf("files = %v, want Main.java and com/acme/a/A.java", paths)
	}
}
