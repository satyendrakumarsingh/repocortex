package infer

import "testing"

func TestLayoutProjectOf(t *testing.T) {
	files := []string{
		"polyglot/backend-go/go.mod",
		"polyglot/backend-go/cmd/main.go",
		"polyglot/worker-python/pyproject.toml",
		"polyglot/worker-python/tasks.py",
		"polyglot/web-ts/package.json",
		"polyglot/web-ts/src/index.ts",
		"polyglot/mobile-dart/pubspec.yaml",
		"polyglot/mobile-dart/lib/main.dart",
		"polyglot/billing-php/composer.json",
		"polyglot/billing-php/src/Charge.php",
		"polyglot/core-rust/Cargo.toml",
		"polyglot/core-rust/src/lib.rs",
		"polyglot/dotnet-app/App.csproj",
		"polyglot/dotnet-app/Program.cs",
		"root-file.go",
	}

	layout := NewLayout(files, nil)

	cases := []struct {
		path string
		want string
	}{
		{"polyglot/backend-go/cmd/main.go", "polyglot/backend-go"},
		{"polyglot/worker-python/tasks.py", "polyglot/worker-python"},
		{"polyglot/web-ts/src/index.ts", "polyglot/web-ts"},
		{"polyglot/mobile-dart/lib/main.dart", "polyglot/mobile-dart"},
		{"polyglot/billing-php/src/Charge.php", "polyglot/billing-php"},
		{"polyglot/core-rust/src/lib.rs", "polyglot/core-rust"},
		{"polyglot/dotnet-app/Program.cs", "polyglot/dotnet-app"},
		{"root-file.go", ""},
		{"unknown/dir/file.py", ""},
	}

	for _, tc := range cases {
		got := layout.ProjectOf(tc.path)
		if got != tc.want {
			t.Errorf("ProjectOf(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// A tsconfig.json or requirements.txt inside a project's src/, tests/ or
// docs/ configures that folder; it does not make the folder a project of its
// own. Counting it split one project in two, so a test file and the code it
// exercises landed in different "subprojects" and lost each other as
// candidates.
func TestLayoutIgnoresSecondaryMarkersInsideConventionalFolders(t *testing.T) {
	layout := NewLayout([]string{
		"web/package.json",
		"web/src/tsconfig.json",
		"web/src/app.ts",
		"web/tests/tsconfig.json",
		"web/tests/app.test.ts",
		"svc/pyproject.toml",
		"svc/docs/requirements.txt",
		"svc/app/main.py",
		"tools/requirements.txt",
		"tools/run.py",
	}, nil)
	for path, want := range map[string]string{
		"web/src/app.ts":        "web",
		"web/tests/app.test.ts": "web",
		"svc/app/main.py":       "svc",
		"tools/run.py":          "tools", // a requirements.txt at a real project's root still counts
	} {
		if got := layout.ProjectOf(path); got != want {
			t.Errorf("ProjectOf(%q) = %q, want %q", path, got, want)
		}
	}
}

// With no project around it, a secondary marker in a conventional folder is
// still a project root: the repository's only marker is src/tsconfig.json.
func TestLayoutKeepsALoneSecondaryMarker(t *testing.T) {
	layout := NewLayout([]string{"app/src/tsconfig.json", "app/src/main.ts", "app/src/util.ts"}, nil)
	if got := layout.ProjectOf("app/src/main.ts"); got != "app/src" {
		t.Errorf("ProjectOf = %q, want app/src", got)
	}
}
