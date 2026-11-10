package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runInfra executes `infra` against a project root.
//
// It prefers the real root command, so that once `infra` is registered in
// root.go these tests exercise the same wiring a user gets. Until then it builds
// the same persistent-flag setup around the command directly — the alternative
// was to leave the command untested until someone else's file changed, which is
// how a command ships broken.
func runInfra(t *testing.T, projectRoot string, args ...string) (string, error) {
	t.Helper()
	if cmd, _, err := NewRoot().Find([]string{"infra"}); err == nil && cmd.Name() == "infra" {
		return run(t, projectRoot, append([]string{"infra"}, args...)...)
	}

	g := &Global{}
	root := &cobra.Command{
		Use: "cortex", SilenceUsage: true, SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return g.setup(cmd) },
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&g.JSON, "json", false, "emit machine-readable JSON on stdout")
	pf.BoolVarP(&g.Verbose, "verbose", "v", false, "verbose logging")
	pf.BoolVarP(&g.Quiet, "quiet", "q", false, "suppress all logging")
	pf.StringVar(&g.Project, "project", "", "project root")
	root.AddCommand(newInfraCmd(g))

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"--project", projectRoot, "--quiet", "infra"}, args...))
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

// infraSecrets are the values planted in the fixture. Not one of them may ever
// appear in output — this is the list every assertion in this file checks
// against.
var infraSecrets = []string{
	"hunter2-super-secret",
	"ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	"aHVudGVyMi1zdXBlci1zZWNyZXQ=",
	"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	"postgres://app:hunter2-super-secret@db:5432/orders",
}

// infraFixture is a repository with one of everything, including credentials in
// every format, because the point of the fixture is to give the command every
// opportunity to leak one.
func infraFixture(t *testing.T) string {
	t.Helper()
	root := isolate(t)

	files := map[string]string{
		"services/api/Dockerfile": `FROM golang:1.25 AS build
RUN go build -o /app ./cmd/api

FROM gcr.io/distroless/base
ENV SERVICE_NAME=orders-api \
    DB_PASSWORD=hunter2-super-secret
EXPOSE 8080
ENTRYPOINT ["/app", "--token=ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]
`,
		"docker-compose.yml": `services:
  api:
    build:
      context: ./services/api
    ports:
      - "8080:8080"
    environment:
      - DATABASE_URL=postgres://app:hunter2-super-secret@db:5432/orders
      - LOG_LEVEL=debug
    depends_on:
      - db
  db:
    image: postgres:16
    environment:
      POSTGRES_PASSWORD: hunter2-super-secret
`,
		"k8s/orders.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders-api
spec:
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/acme/orders:1.4.0
          ports:
            - containerPort: 8080
          env:
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: db-creds
                  key: password
          envFrom:
            - secretRef:
                name: platform-creds
---
apiVersion: v1
kind: Secret
metadata:
  name: db-creds
data:
  password: aHVudGVyMi1zdXBlci1zZWNyZXQ=
`,
		"k8s/broken.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: [unclosed\n",
		"infra/main.tf": `resource "aws_db_instance" "main" {
  password = "hunter2-super-secret"
}

provider "aws" {
  secret_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
}

variable "db_password" {}
`,
		"charts/api/Chart.yaml":                "name: api\nversion: 0.1.0\nappVersion: \"1.4.0\"\n",
		"charts/api/values.yaml":               "replicaCount: 2\nauth:\n  password: hunter2-super-secret\n",
		"charts/api/templates/deployment.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ .Release.Name }}\n",
		".github/workflows/ci.yml":             "jobs:\n  test:\n    steps:\n      - run: go test ./...\n",
		"README.md":                            "# fixture\n",
	}
	for path, body := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestInfraNeverPrintsAValue is the security test for the whole feature.
//
// Spec §24 requires that secret values are never exposed. The guarantee is
// structural — the extractor has no field a value fits in — and this asserts it
// at the only place a user would ever see one.
func TestInfraNeverPrintsAValue(t *testing.T) {
	root := infraFixture(t)

	// Both report paths, because they are two code paths to one guarantee.
	// The disk path renders what the extractor just produced; the index path
	// renders what was WRITTEN TO DISK and read back, which is the one a value
	// would have to survive a database round trip to reach.
	indexed := infraIndexedFixture(t)
	for _, args := range [][]string{
		{"--disk"}, {"--disk", "--keys"}, {"--disk", "--json"}, {"--disk", "--keys", "--json"},
		{"--disk", "--format", "kubernetes"}, {"--disk", "--format", "terraform"},
	} {
		out, err := runInfra(t, root, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		for _, secret := range infraSecrets {
			if strings.Contains(out, secret) {
				t.Errorf("infra %v LEAKED a value %q:\n%s", args, secret, out)
			}
		}
	}

	for _, args := range [][]string{
		{}, {"--keys"}, {"--json"}, {"--keys", "--json"},
		{"--key", "DB_PASSWORD"}, {"--for", "services/api/main.go"},
	} {
		out, err := runInfra(t, indexed, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		for _, secret := range infraSecrets {
			if strings.Contains(out, secret) {
				t.Errorf("infra %v LEAKED a value FROM THE INDEX %q:\n%s", args, secret, out)
			}
		}
	}

	// And the database file itself, which is what a backup, a sync client or a
	// future tool nobody has written yet would read. A value that never
	// reaches the file cannot leak from it later.
	db, err := os.ReadFile(filepath.Join(indexed, IndexDir, IndexFile))
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	for _, secret := range infraSecrets {
		if bytes.Contains(db, []byte(secret)) {
			t.Errorf("the index FILE contains the value %q", secret)
		}
	}
}

// infraIndexedFixture is the same repository, indexed, so the tests that care
// about the index have one.
func infraIndexedFixture(t *testing.T) string {
	t.Helper()
	root := infraFixture(t)
	for _, args := range [][]string{{"init"}, {"index"}, {"resolve"}} {
		if out, err := run(t, root, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return root
}

func TestInfraReportsEveryFormat(t *testing.T) {
	root := infraFixture(t)
	out, err := runInfra(t, root, "--disk")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	for _, want := range []string{
		"services/api/Dockerfile", "docker-compose.yml", "k8s/orders.yaml",
		"infra/main.tf", "charts/api/values.yaml", "charts/api/Chart.yaml",
		// The link from a service back to the directory it is built from.
		"built from ./services/api",
		// Names and sources, never values.
		"resource:aws_db_instance", "ghcr.io/acme/orders:1.4.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}

	// A CI workflow is YAML and is not a deployment manifest. Reporting one
	// would make every repository look like it deploys something.
	if strings.Contains(out, "workflows/ci.yml") {
		t.Errorf("a CI workflow was reported as a manifest:\n%s", out)
	}
}

// TestInfraReportsWhatItCouldNotRead is the honesty test. "No manifests found"
// and "your manifests could not be parsed" have opposite consequences.
func TestInfraReportsWhatItCouldNotRead(t *testing.T) {
	root := infraFixture(t)
	out, err := runInfra(t, root, "--disk")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	if !strings.Contains(out, "COULD NOT READ") {
		t.Fatalf("the unreadable-manifest section is missing:\n%s", out)
	}
	if !strings.Contains(out, "k8s/broken.yaml") || !strings.Contains(out, "malformed_yaml") {
		t.Errorf("a manifest that failed to parse was not named:\n%s", out)
	}
	if !strings.Contains(out, "templates/deployment.yaml") ||
		!strings.Contains(out, "templated_not_rendered") {
		t.Errorf("an unrendered Helm template must be reported, not silently skipped:\n%s", out)
	}
}

// TestInfraReportsConsumedButUndeclared covers the cross-file finding: a whole
// Secret injected into a container that no manifest here declares.
func TestInfraReportsConsumedButUndeclared(t *testing.T) {
	root := infraFixture(t)
	out, err := runInfra(t, root, "--disk")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "CONSUMED BUT NOT DECLARED HERE") ||
		!strings.Contains(out, "platform-creds") {
		t.Errorf("an envFrom Secret nothing declares was not reported:\n%s", out)
	}
	// db-creds IS declared in the same file, so flagging it would be crying wolf.
	if strings.Contains(out, "Secret db-creds\tconsumed") {
		t.Errorf("a Secret declared right there was reported as missing:\n%s", out)
	}
}

// TestInfraNamesWhichSourceAnsweredIt.
//
// This used to assert the opposite: that the report says NOT INDEXED. It now
// asserts the thing that replaced that caveat — the report says which of the
// two sources produced it. A stale index and a fresh disk scan disagree, and a
// reader who cannot tell them apart cannot tell a stale answer from a current
// one, which is the same failure the old caveat existed to prevent.
func TestInfraNamesWhichSourceAnsweredIt(t *testing.T) {
	root := infraIndexedFixture(t)

	out, err := runInfra(t, root)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "Read from the index") {
		t.Errorf("the indexed report must say so:\n%s", out)
	}
	if strings.Contains(out, "NOT INDEXED") {
		t.Errorf("these facts ARE indexed now:\n%s", out)
	}

	disk, err := runInfra(t, root, "--disk")
	if err != nil {
		t.Fatalf("%v\n%s", err, disk)
	}
	if !strings.Contains(disk, "Read from disk just now") {
		t.Errorf("the disk report must say so:\n%s", disk)
	}

	jsonOut, err := runInfra(t, root, "--json")
	if err != nil {
		t.Fatalf("%v\n%s", err, jsonOut)
	}
	var payload struct {
		Source  string `json:"source"`
		Summary struct {
			Files      int `json:"files"`
			Units      int `json:"units"`
			EnvKeys    int `json:"env_keys"`
			SecretKeys int `json:"secret_keys"`
			Problems   int `json:"problems"`
		} `json:"summary"`
		Files []struct {
			Path   string `json:"path"`
			Format string `json:"format"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, jsonOut)
	}
	if payload.Source != "index" {
		t.Errorf("a machine-readable caller must be told which source answered, got %q", payload.Source)
	}
	if payload.Summary.Files < 6 || payload.Summary.Units == 0 {
		t.Errorf("summary looks wrong: %+v", payload.Summary)
	}
	if payload.Summary.SecretKeys == 0 {
		t.Error("the fixture is full of sensitive key names; none were labelled")
	}
	// The manifest that failed to parse has to survive INTO the index. A run
	// that stored only the manifests it understood would report this
	// repository's deployment as smaller and healthier than it is.
	if payload.Summary.Problems == 0 {
		t.Errorf("an unreadable manifest did not survive indexing: %+v", payload.Summary)
	}
}

// TestInfraAnswersWhichServiceCodeRunsIn is the join the persistence hop
// exists for. No line of Go says which service it ships in; a compose file's
// build context does, and only once both are nodes in one graph can the two be
// put together.
func TestInfraAnswersWhichServiceCodeRunsIn(t *testing.T) {
	root := infraIndexedFixture(t)

	out, err := runInfra(t, root, "--for", "services/api/main.go")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "runs in") || !strings.Contains(out, "build context services/api") {
		t.Errorf("the service a file runs in was not named:\n%s", out)
	}

	// A file no build context covers must produce an empty answer that says
	// WHY, not a service picked because it was the only candidate.
	none, err := runInfra(t, root, "--for", "README.md")
	if err != nil {
		t.Fatalf("%v\n%s", err, none)
	}
	if !strings.Contains(none, "No deployment unit") {
		t.Errorf("an uncovered file must be told so plainly:\n%s", none)
	}
}

// TestInfraAnswersWhatAConfigKeyRestarts is the second join, and the one that
// needs a key name to be the SAME node whether a manifest declared it or code
// read it.
func TestInfraAnswersWhatAConfigKeyRestarts(t *testing.T) {
	root := infraIndexedFixture(t)

	out, err := runInfra(t, root, "--key", "DB_PASSWORD")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "restarts") {
		t.Errorf("no workload was named for a key the fixture sets:\n%s", out)
	}
	if !strings.Contains(out, "credential") {
		t.Errorf("a key whose name says credential must be labelled:\n%s", out)
	}

	missing, err := runInfra(t, root, "--key", "NOTHING_SETS_THIS")
	if err != nil {
		t.Fatalf("%v\n%s", err, missing)
	}
	if !strings.Contains(missing, "No indexed deployment unit sets") {
		t.Errorf("an empty answer must not read as a failure:\n%s", missing)
	}
	// An empty answer that does not say a Secret from outside the repository
	// is invisible here reads as "nothing uses this key", which is a different
	// and much more dangerous claim.
	if !strings.Contains(missing, "outside this repository") {
		t.Errorf("the empty answer must say what it cannot see:\n%s", missing)
	}
}

// TestInfraWithoutManifests: a repository that deploys nothing must be told so
// in a way that does not read as a failure, and must say what was looked for.
func TestInfraWithoutManifests(t *testing.T) {
	root := isolate(t)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runInfra(t, root, "--disk")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "No deployment manifests found") {
		t.Errorf("unexpected output:\n%s", out)
	}
	if !strings.Contains(out, "coverage limit") {
		t.Errorf("an empty result must say what was looked for:\n%s", out)
	}
}

// TestInfraDiskNeedsNoIndex: this is the report you want on a repository you
// just cloned, and it is why the disk path was kept rather than deleted when
// the facts became indexable.
//
// The second half is the part that matters more than it looks. Without an
// index the command must FAIL rather than quietly scanning the disk, because a
// silent fallback makes "these facts are in your graph" and "these facts were
// read a second ago and stored nowhere" produce identical output — and the
// error has to name the flag, or the advice is useless.
func TestInfraDiskNeedsNoIndex(t *testing.T) {
	root := infraFixture(t)
	if _, err := os.Stat(filepath.Join(root, IndexDir, IndexFile)); err == nil {
		t.Fatal("fixture unexpectedly has an index; this test is meaningless")
	}
	if out, err := runInfra(t, root, "--disk"); err != nil {
		t.Fatalf("infra --disk must work without an index: %v\n%s", err, out)
	}

	out, err := runInfra(t, root)
	if err == nil {
		t.Fatalf("infra without an index must not silently scan the disk:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--disk") {
		t.Errorf("the error must name the flag that works here: %v", err)
	}
}
