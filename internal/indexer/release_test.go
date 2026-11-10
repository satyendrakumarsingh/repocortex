package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

// The exact declaration shapes that appear in real build files. cpam's pom
// carries all three Maven spellings at once, which is why the highest wins
// rather than the first found.
func TestRequiredReleaseReadsRealDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name, file, body string
		want             int
	}{
		{"maven release", "pom.xml", `<maven.compiler.release>21</maven.compiler.release>`, 21},
		{"maven source/target/java.version", "pom.xml", `
			<java.version>25</java.version>
			<maven.compiler.source>25</maven.compiler.source>
			<maven.compiler.target>25</maven.compiler.target>`, 25},
		{"maven 1.8 form", "pom.xml", `<maven.compiler.target>1.8</maven.compiler.target>`, 8},
		{"gradle sourceCompatibility", "build.gradle", `sourceCompatibility = '17'`, 17},
		{"gradle JavaVersion", "build.gradle", `sourceCompatibility = JavaVersion.VERSION_21`, 21},
		{"gradle toolchain kts", "build.gradle.kts", `languageVersion = JavaLanguageVersion.of(25)`, 25},
		{"declares nothing", "pom.xml", `<groupId>com.example</groupId>`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := requiredRelease(dir); got != tc.want {
				t.Errorf("requiredRelease = %d, want %d", got, tc.want)
			}
		})
	}
}

// A version-shaped number that is not a Java release must not block a project
// that would have built.
func TestRequiredReleaseIgnoresOutOfRangeNumbers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pom.xml"), []byte(
		`<java.version>2024</java.version><maven.compiler.source>3</maven.compiler.source>`), 0o600)
	if got := requiredRelease(dir); got != 0 {
		t.Errorf("requiredRelease = %d; 2024 and 3 are not Java releases and must be ignored", got)
	}
}

// The block fires only when both numbers are known and the project is out of
// range. Every other combination must pass, because a false block refuses a
// project that works -- worse than the bug this fixes.
func TestReleaseMismatchFailsOpen(t *testing.T) {
	java17 := Pin{Indexer: SCIPJava, MaxLanguageRelease: 17}

	withPom := func(body string) string {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "pom.xml"), []byte(body), 0o600)
		return dir
	}

	if why := releaseMismatch(java17, withPom(`<maven.compiler.release>25</maven.compiler.release>`)); why == "" {
		t.Error("Java 25 against a JDK-17 image must block: this is the seven-minute build failure")
	}
	if why := releaseMismatch(java17, withPom(`<maven.compiler.release>17</maven.compiler.release>`)); why != "" {
		t.Errorf("equal versions must pass, got %q", why)
	}
	if why := releaseMismatch(java17, withPom(`<groupId>x</groupId>`)); why != "" {
		t.Errorf("a project declaring nothing must not be blocked, got %q", why)
	}
	if why := releaseMismatch(Pin{Indexer: SCIPGo}, withPom(`<maven.compiler.release>25</maven.compiler.release>`)); why != "" {
		t.Errorf("a pin with no declared ceiling must block nothing, got %q", why)
	}
}

// The pin's ceiling is a claim about a specific image digest. If someone
// repoints the image without re-verifying, this is the reminder.
func TestJavaPinDeclaresItsCeiling(t *testing.T) {
	for _, p := range Pins() {
		if p.Language == LangJava && p.MaxLanguageRelease == 0 {
			t.Errorf("%s declares no MaxLanguageRelease; preflight cannot warn and users find out from a failed build", p.Indexer)
		}
	}
}

// The dependency advisory fires exactly when third-party imports would silently
// fail to resolve, and is quiet otherwise. Measured basis: the same TypeScript
// project indexes to 675 bytes without node_modules and 732 with, both exiting
// 0 with "done".
func TestDependencyAdvisory(t *testing.T) {
	ts := Project{Language: LangTypeScript}
	py := Project{Language: LangPython}
	write := func(dir, name string) { os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600) }

	t.Run("typescript without node_modules warns", func(t *testing.T) {
		d := t.TempDir()
		write(d, "package.json")
		if dependencyAdvisory(ts, d) == "" {
			t.Error("declared dependencies with no node_modules must warn: the index is smaller and nothing says so")
		}
	})
	t.Run("typescript with node_modules is quiet", func(t *testing.T) {
		d := t.TempDir()
		write(d, "package.json")
		os.Mkdir(filepath.Join(d, "node_modules"), 0o755)
		if got := dependencyAdvisory(ts, d); got != "" {
			t.Errorf("must be quiet once installed, got %q", got)
		}
	})
	t.Run("no package.json is quiet", func(t *testing.T) {
		if got := dependencyAdvisory(ts, t.TempDir()); got != "" {
			t.Errorf("a project declaring no dependencies has nothing to warn about, got %q", got)
		}
	})
	t.Run("python venv suppresses", func(t *testing.T) {
		d := t.TempDir()
		write(d, "requirements.txt")
		if dependencyAdvisory(py, d) == "" {
			t.Error("declared python deps with no local env must warn")
		}
		os.Mkdir(filepath.Join(d, ".venv"), 0o755)
		if got := dependencyAdvisory(py, d); got != "" {
			t.Errorf("a local virtualenv must suppress it, got %q", got)
		}
	})
	t.Run("go is out of scope", func(t *testing.T) {
		d := t.TempDir()
		write(d, "package.json")
		if got := dependencyAdvisory(Project{Language: LangGo}, d); got != "" {
			t.Errorf("scip-go resolves through the module cache, got %q", got)
		}
	})
}
