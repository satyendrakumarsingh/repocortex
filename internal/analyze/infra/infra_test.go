package infra

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		path string
		want class
	}{
		{"Dockerfile", classDockerfile},
		{"build/Dockerfile.prod", classDockerfile},
		{"svc/api.dockerfile", classDockerfile},
		{"Containerfile", classDockerfile},
		{"docker-compose.yml", classCompose},
		{"deploy/docker-compose.prod.yaml", classCompose},
		{"compose.yaml", classCompose},
		{"infra/main.tf", classTerraform},
		{"charts/api/Chart.yaml", classHelmChart},
		{"charts/api/values.yaml", classHelmValues},
		{"charts/api/values-prod.yaml", classHelmValues},
		{"charts/api/templates/deployment.yaml", classHelmTemplate},
		{"charts/api/templates/_helpers.tpl", classHelmTemplate},
		{"k8s/deployment.yaml", classYAML},
		// Not manifests. A .tpl outside a chart is anybody's template language,
		// and a lone .tfvars holds values this package must never read.
		{"README.md", classNone},
		{"web/index.tpl", classNone},
		{"infra/prod.tfvars", classNone},
		{"main.go", classNone},
	}
	for _, c := range cases {
		if got := classify(c.path); got != c.want {
			t.Errorf("classify(%q) = %v, want %v", c.path, got, c.want)
		}
		if Candidate(c.path) != (c.want != classNone) {
			t.Errorf("Candidate(%q) disagrees with classify", c.path)
		}
	}
}

const dockerfileSrc = `# syntax=docker/dockerfile:1
ARG GO_VERSION=1.25
FROM golang:${GO_VERSION} AS build
WORKDIR /src
RUN go build -o /app ./cmd/api

FROM gcr.io/distroless/base AS runtime
ENV SERVICE_NAME=api \
    DB_PASSWORD=hunter2-super-secret \
    LOG_LEVEL=info
# a comment inside the instruction above
ENV LEGACY_KEY legacy value with spaces
EXPOSE 8080 9090/udp ${METRICS_PORT}
ENTRYPOINT ["/app", "--token=ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]
CMD ["--verbose"]
`

func TestDockerfile(t *testing.T) {
	f, ok := Parse("Dockerfile", []byte(dockerfileSrc))
	if !ok {
		t.Fatal("Dockerfile not recognised")
	}
	if len(f.Units) != 2 {
		t.Fatalf("got %d stages, want 2: %+v", len(f.Units), f.Units)
	}

	build, runtime := f.Units[0], f.Units[1]
	if build.Name != "build" || build.Image != "golang:${GO_VERSION}" {
		t.Errorf("stage 0 = %q image %q", build.Name, build.Image)
	}
	if build.Final {
		t.Error("the first of two stages is not the final image")
	}
	if !runtime.Final {
		t.Error("the last stage builds the image that runs; it must be marked final")
	}
	// A global ARG belongs to the first stage, not to nothing.
	if !hasKey(build.Env, "GO_VERSION") {
		t.Errorf("global ARG lost: %+v", build.Env)
	}

	for _, want := range []string{"SERVICE_NAME", "DB_PASSWORD", "LOG_LEVEL", "LEGACY_KEY"} {
		if !hasKey(runtime.Env, want) {
			t.Errorf("ENV key %q missing: %+v", want, runtime.Env)
		}
	}
	if k, ok := findKey(runtime.Env, "DB_PASSWORD"); !ok || !k.Secret {
		t.Errorf("DB_PASSWORD must be labelled sensitive by name: %+v", k)
	}

	ports := runtime.Ports
	if len(ports) != 3 {
		t.Fatalf("got %d ports, want 3: %+v", len(ports), ports)
	}
	if ports[0].Container != 8080 || ports[1].Container != 9090 || ports[1].Protocol != "udp" {
		t.Errorf("ports read wrong: %+v", ports)
	}
	if ports[2].Raw != "${METRICS_PORT}" {
		t.Errorf("a variable port must survive as Raw, got %+v", ports[2])
	}
	if !strings.Contains(runtime.Command, "[REDACTED:") {
		t.Errorf("the token in ENTRYPOINT was not redacted: %q", runtime.Command)
	}
}

func TestDockerfileWithoutFromIsReported(t *testing.T) {
	f, ok := Parse("Dockerfile", []byte("RUN echo hello\nEXPOSE 80\n"))
	if !ok {
		t.Fatal("not recognised")
	}
	if len(f.Units) != 0 {
		t.Fatalf("a Dockerfile with no FROM declares no image: %+v", f.Units)
	}
	if !hasProblem(f, ReasonNoFrom) {
		t.Errorf("silence about a Dockerfile that builds nothing: %+v", f.Problems)
	}
}

func TestDockerfileEscapeDirective(t *testing.T) {
	src := "# escape=`\nFROM alpine\nENV A=1 `\n    B=2\n"
	f, _ := Parse("Dockerfile", []byte(src))
	if len(f.Units) != 1 || !hasKey(f.Units[0].Env, "B") {
		t.Fatalf("backtick continuation not honoured: %+v", f.Units)
	}
}

const composeSrc = `
x-common: &common
  restart: always
  environment:
    SHARED_TOKEN: abc123
services:
  api:
    <<: *common
    build:
      context: ./services/api
      dockerfile: Dockerfile.prod
    ports:
      - "8080:8080"
      - "127.0.0.1:9090:9090/tcp"
      - "${METRICS}:9100"
    environment:
      - DATABASE_URL=postgres://app:hunter2@db:5432/orders
      - LOG_LEVEL=debug
      - HOME
    env_file:
      - deploy/prod.env
    depends_on:
      - db
    deploy:
      resources:
        limits:
          cpus: "0.5"
          memory: 512M
  db:
    <<: *common
    image: postgres:16
    expose:
      - 5432
secrets:
  db_password:
    file: ./secrets/db_password.txt
`

func TestCompose(t *testing.T) {
	f, ok := Parse("docker-compose.yml", []byte(composeSrc))
	if !ok {
		t.Fatal("compose file not recognised")
	}
	if len(f.Units) != 2 {
		t.Fatalf("got %d services, want 2: %+v", len(f.Units), f.Units)
	}
	api := f.Units[0]
	if api.Name != "api" || api.BuildContext != "./services/api" || api.Dockerfile != "Dockerfile.prod" {
		t.Errorf("build context is the link back to source, got %+v", api)
	}
	if len(api.Ports) != 3 {
		t.Fatalf("ports: %+v", api.Ports)
	}
	if api.Ports[0].Published != 8080 || api.Ports[0].Container != 8080 {
		t.Errorf("short form port: %+v", api.Ports[0])
	}
	if api.Ports[1].Published != 9090 || api.Ports[1].Container != 9090 {
		t.Errorf("host-ip form port: %+v", api.Ports[1])
	}
	if api.Ports[2].Raw == "" {
		t.Errorf("a variable published port must survive as Raw: %+v", api.Ports[2])
	}
	for _, want := range []string{"DATABASE_URL", "LOG_LEVEL", "HOME"} {
		if !hasKey(api.Env, want) {
			t.Errorf("env key %q missing: %+v", want, api.Env)
		}
	}
	// YAML merge keys do not deep-merge: a service that declares its own
	// `environment` replaces the anchor's outright, and reporting the anchor's
	// keys as well would claim this container is handed a variable it is not.
	if hasKey(api.Env, "SHARED_TOKEN") {
		t.Errorf("a merged key must not survive an override: %+v", api.Env)
	}
	if !hasKey(f.Units[1].Env, "SHARED_TOKEN") {
		t.Errorf("merge key not expanded for a service that overrides nothing: %+v", f.Units[1].Env)
	}
	if k, _ := findKey(api.Env, "HOME"); k.From != SourceHost {
		t.Errorf("a bare name takes its value from the host: %+v", k)
	}
	if len(api.EnvFiles) != 1 || api.EnvFiles[0] != "deploy/prod.env" {
		t.Errorf("env_file: %+v", api.EnvFiles)
	}
	if len(api.DependsOn) != 1 || api.DependsOn[0] != "db" {
		t.Errorf("depends_on: %+v", api.DependsOn)
	}
	if len(api.Limits) != 2 {
		t.Errorf("deploy limits: %+v", api.Limits)
	}
	if len(f.Units[1].Ports) != 1 || f.Units[1].Ports[0].Published != 0 {
		t.Errorf("expose publishes nothing to the host: %+v", f.Units[1].Ports)
	}
	if len(f.KeySets) != 1 || f.KeySets[0].Name != "db_password" || !f.KeySets[0].Secret {
		t.Errorf("top-level secrets: %+v", f.KeySets)
	}
}

const k8sSrc = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: orders-api
  namespace: prod
spec:
  replicas: 3
  template:
    spec:
      initContainers:
        - name: migrate
          image: ghcr.io/acme/migrate:1.2
      containers:
        - name: app
          image: ghcr.io/acme/orders:1.4.0
          command: ["/app", "--api-key=AIzaSyAbcdefghijklmnopqrstuvwxyz012345678"]
          ports:
            - containerPort: 8080
              name: http
          env:
            - name: LOG_LEVEL
              value: debug
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: db-creds
                  key: password
            - name: FEATURE_FLAGS
              valueFrom:
                configMapKeyRef:
                  name: orders-config
                  key: flags
            - name: POD_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.podIP
          envFrom:
            - secretRef:
                name: shared-creds
            - configMapRef:
                name: orders-config
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: "1"
              memory: 512Mi
---
apiVersion: v1
kind: Service
metadata:
  name: orders-api
spec:
  ports:
    - port: 80
      targetPort: 8080
      name: http
---
apiVersion: v1
kind: Secret
metadata:
  name: db-creds
type: Opaque
data:
  password: aHVudGVyMi1zdXBlci1zZWNyZXQ=
  username: YXBw
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: orders-config
data:
  flags: "new-checkout=true"
`

func TestKubernetes(t *testing.T) {
	f, ok := Parse("k8s/orders.yaml", []byte(k8sSrc))
	if !ok {
		t.Fatal("Kubernetes manifest not recognised")
	}
	if f.Format != FormatKubernetes {
		t.Fatalf("format = %q", f.Format)
	}
	if len(f.Problems) != 0 {
		t.Fatalf("unexpected problems: %+v", f.Problems)
	}

	// init container, app container, Service
	if len(f.Units) != 3 {
		t.Fatalf("got %d units, want 3: %+v", len(f.Units), unitNames(f))
	}
	init, app, svc := f.Units[0], f.Units[1], f.Units[2]
	if init.Type != "Deployment/init" {
		t.Errorf("an init container is not the workload's serving container: %q", init.Type)
	}
	if app.Name != "prod/orders-api/app" {
		t.Errorf("unit name should locate the container inside its workload: %q", app.Name)
	}
	if app.Image != "ghcr.io/acme/orders:1.4.0" {
		t.Errorf("image = %q", app.Image)
	}
	if len(app.Ports) != 1 || app.Ports[0].Container != 8080 || app.Ports[0].Name != "http" {
		t.Errorf("container ports: %+v", app.Ports)
	}
	if !strings.Contains(app.Command, "[REDACTED:") {
		t.Errorf("the Google API key in command/args was not redacted: %q", app.Command)
	}

	k, ok := findKey(app.Env, "DB_PASSWORD")
	if !ok || k.From != SourceSecret || k.Ref != "db-creds:password" || !k.Secret {
		t.Errorf("secretKeyRef not read as one: %+v", k)
	}
	if k, _ := findKey(app.Env, "FEATURE_FLAGS"); k.From != SourceConfigMap || k.Ref != "orders-config:flags" {
		t.Errorf("configMapKeyRef: %+v", k)
	}
	if k, _ := findKey(app.Env, "POD_IP"); k.From != SourceDownward || k.Ref != "status.podIP" {
		t.Errorf("fieldRef: %+v", k)
	}
	if k, _ := findKey(app.Env, "LOG_LEVEL"); k.From != SourceLiteral || k.Secret {
		t.Errorf("a plain literal key: %+v", k)
	}
	if len(app.EnvFrom) != 2 {
		t.Fatalf("envFrom: %+v", app.EnvFrom)
	}
	if app.EnvFrom[0].Kind != "Secret" || app.EnvFrom[0].Name != "shared-creds" {
		t.Errorf("envFrom secretRef: %+v", app.EnvFrom[0])
	}
	if len(app.Limits) != 2 {
		t.Fatalf("resources: %+v", app.Limits)
	}
	if app.Limits[0].Resource != "cpu" || app.Limits[0].Request != "100m" || app.Limits[0].Limit != "1" {
		t.Errorf("cpu request/limit: %+v", app.Limits[0])
	}
	if svc.Type != "Service" || len(svc.Ports) != 1 ||
		svc.Ports[0].Published != 80 || svc.Ports[0].Container != 8080 {
		t.Errorf("Service ports: %+v", svc)
	}

	if len(f.KeySets) != 2 {
		t.Fatalf("ConfigMap and Secret both declare keys: %+v", f.KeySets)
	}
	secret := f.KeySets[0]
	if secret.Type != "Secret" || !secret.Secret || len(secret.Keys) != 2 {
		t.Fatalf("Secret key set: %+v", secret)
	}
	for _, k := range secret.Keys {
		if !k.Secret {
			t.Errorf("every key of a Secret is sensitive whatever it is called: %+v", k)
		}
	}
}

// TestUnresolvedEnvFrom is the cross-file finding: a whole Secret injected into
// a container that no manifest in the scan declares.
func TestUnresolvedEnvFrom(t *testing.T) {
	f, _ := Parse("k8s/orders.yaml", []byte(k8sSrc))
	s := Summarize([]File{f})

	if len(s.Unresolved) != 1 {
		t.Fatalf("want exactly the Secret nothing declares, got %+v", s.Unresolved)
	}
	if s.Unresolved[0].Name != "shared-creds" || s.Unresolved[0].Kind != "Secret" {
		t.Errorf("unresolved = %+v", s.Unresolved[0])
	}
	// db-creds and orders-config ARE declared in the same file, so neither is a
	// finding — a report that cried wolf about them would be ignored.
	if s.Units != 3 || s.KeySets != 2 {
		t.Errorf("summary counts: %+v", s)
	}
}

func TestMalformedYAMLIsReportedNotSkipped(t *testing.T) {
	src := "apiVersion: v1\nkind: Service\nmetadata:\n  name: api\n  ports: [ unclosed\n"
	f, ok := Parse("k8s/broken.yaml", []byte(src))
	if !ok {
		t.Fatal(`a file that says "apiVersion:" and does not parse must still be reported`)
	}
	if !hasProblem(f, ReasonMalformedYAML) {
		t.Fatalf("no problem recorded: %+v", f)
	}
	for _, p := range f.Problems {
		if strings.Contains(p.Detail, "unclosed") && !strings.Contains(p.Detail, "REDACTED") {
			// The detail is the parser's message, which quotes document text.
			// Quoting is fine; quoting a credential is not, which is why it goes
			// through the redactor.
			t.Logf("parser message quotes document text: %q", p.Detail)
		}
	}
}

func TestPlainYAMLIsNotAManifest(t *testing.T) {
	src := "jobs:\n  build:\n    steps:\n      - run: make test\n"
	if f, ok := Parse(".github/workflows/ci.yaml", []byte(src)); ok {
		t.Fatalf("a CI workflow is not a deployment manifest: %+v", f)
	}
}

func TestHelmValuesAndTemplates(t *testing.T) {
	values := "replicaCount: 2\npostgresql:\n  auth:\n    password: hunter2\n    username: app\n"
	f, ok := Parse("charts/api/values.yaml", []byte(values))
	if !ok {
		t.Fatal("values.yaml not recognised")
	}
	if len(f.KeySets) != 1 {
		t.Fatalf("values: %+v", f.KeySets)
	}
	names := map[string]bool{}
	for _, k := range f.KeySets[0].Keys {
		names[k.Name] = true
	}
	for _, want := range []string{"replicaCount", "postgresql", "postgresql.auth", "postgresql.auth.password"} {
		if !names[want] {
			t.Errorf("dotted key %q missing: %v", want, names)
		}
	}
	if k, _ := findKey(f.KeySets[0].Keys, "postgresql.auth.password"); !k.Secret {
		t.Error("a values key whose name says password is sensitive")
	}

	tmpl := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ .Release.Name }}-api\n"
	tf, ok := Parse("charts/api/templates/deployment.yaml", []byte(tmpl))
	if !ok {
		t.Fatal("helm template not recognised")
	}
	if len(tf.Units) != 0 {
		t.Errorf("a template must not be reported as declaring objects: %+v", tf.Units)
	}
	if !hasProblem(tf, ReasonTemplated) {
		t.Errorf("an unrendered template must be reported: %+v", tf.Problems)
	}
}

const terraformSrc = `
# a comment with a { brace
provider "aws" {
  region = "us-east-1"
  secret_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
}

resource "aws_db_instance" "main" {
  password = "hunter2-super-secret"
  tags = {
    Name = "orders"
  }
}

resource "aws_iam_policy" "app" {
  policy = <<EOT
{
  "Statement": [{"Effect": "Allow"}]
EOT
}

variable "db_password" {
  sensitive = true
}

variable "region" {}

module "network" {
  source = "./modules/network"
}

output "endpoint" {
  value = "${aws_db_instance.main.endpoint}"
}
`

func TestTerraform(t *testing.T) {
	f, ok := Parse("infra/main.tf", []byte(terraformSrc))
	if !ok {
		t.Fatal(".tf not recognised")
	}
	if len(f.Problems) != 0 {
		t.Fatalf("brace tracking drifted: %+v", f.Problems)
	}

	types := map[string]string{}
	for _, u := range f.Units {
		types[u.Type+"."+u.Name] = u.Name
	}
	for _, want := range []string{
		"provider.aws", "resource:aws_db_instance.main",
		"resource:aws_iam_policy.app", "module.network",
	} {
		if _, ok := types[want]; !ok {
			t.Errorf("block %q missing: %v", want, types)
		}
	}
	if _, ok := types["output.endpoint"]; ok {
		t.Error("an output is not a deployed thing and must not pad the inventory")
	}
	if len(f.KeySets) != 1 || len(f.KeySets[0].Keys) != 2 {
		t.Fatalf("variables: %+v", f.KeySets)
	}
	if k, _ := findKey(f.KeySets[0].Keys, "db_password"); !k.Secret {
		t.Error("a variable whose name says password is sensitive")
	}
}

// TestNoValueIsEverEmitted is the security test for the whole package.
//
// Every fixture below carries a distinctive value next to a key. The assertion
// is made against the SERIALISED result, because that is what a caller renders
// and what an MCP response would carry — checking the fields individually would
// pass while a value sat in some field nobody thought to look at.
func TestNoValueIsEverEmitted(t *testing.T) {
	cases := []struct {
		path, src string
		secrets   []string
	}{
		{
			"Dockerfile",
			"FROM alpine\nENV DB_PASSWORD=hunter2-super-secret API_TOKEN=ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
				"ARG NPM_TOKEN=npm_zzzzzzzzzzzzzzzzzzzz\nENTRYPOINT [\"/app\",\"--password=hunter2-super-secret\"]\n",
			[]string{"hunter2-super-secret", "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "npm_zzzzzzzzzzzzzzzzzzzz"},
		},
		{
			"docker-compose.yml",
			"services:\n  api:\n    image: api\n    environment:\n" +
				"      DB_PASSWORD: hunter2-super-secret\n      URL: postgres://app:hunter2-super-secret@db/o\n" +
				"    command: [\"serve\",\"--token=ghp_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"]\n",
			[]string{"hunter2-super-secret", "ghp_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
		{
			"k8s/secret.yaml",
			"apiVersion: v1\nkind: Secret\nmetadata:\n  name: creds\ndata:\n" +
				"  password: aHVudGVyMi1zdXBlci1zZWNyZXQ=\nstringData:\n  token: ghp_cccccccccccccccccccccccccccccccccccc\n",
			[]string{"aHVudGVyMi1zdXBlci1zZWNyZXQ=", "ghp_cccccccccccccccccccccccccccccccccccc"},
		},
		{
			"k8s/deploy.yaml",
			"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  template:\n    spec:\n" +
				"      containers:\n        - name: app\n          image: api\n          env:\n" +
				"            - name: DB_PASSWORD\n              value: hunter2-super-secret\n",
			[]string{"hunter2-super-secret"},
		},
		{
			"charts/api/values.yaml",
			"auth:\n  password: hunter2-super-secret\n  awsSecretKey: wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n",
			[]string{"hunter2-super-secret", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		},
		{
			"infra/main.tf",
			"resource \"aws_db_instance\" \"main\" {\n  password = \"hunter2-super-secret\"\n}\n" +
				"provider \"aws\" {\n  secret_key = \"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\"\n}\n",
			[]string{"hunter2-super-secret", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		},
	}

	for _, c := range cases {
		f, ok := Parse(c.path, []byte(c.src))
		if !ok {
			t.Fatalf("%s: not recognised", c.path)
		}
		blob, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		for _, secret := range c.secrets {
			if strings.Contains(string(blob), secret) {
				t.Errorf("%s LEAKED a value %q:\n%s", c.path, secret, blob)
			}
		}
		// And the keys must still be there: a scanner that leaks nothing because
		// it reads nothing is not the goal.
		if len(blob) < 20 {
			t.Errorf("%s produced nothing at all: %s", c.path, blob)
		}
	}
}

// TestSecretAsKeyName covers the inversion: an allow-list keyed by credential
// puts the secret on the LEFT of the delimiter, which is the half this package
// keeps.
func TestSecretAsKeyName(t *testing.T) {
	src := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: allow\ndata:\n" +
		"  ghp_dddddddddddddddddddddddddddddddddddd: allowed\n"
	f, ok := Parse("k8s/allow.yaml", []byte(src))
	if !ok {
		t.Fatal("not recognised")
	}
	blob, _ := json.Marshal(f)
	if strings.Contains(string(blob), "ghp_dddddddddddddddddddddddddddddddddddd") {
		t.Errorf("a credential used as a KEY reached the output:\n%s", blob)
	}
	if !strings.Contains(string(blob), "REDACTED") {
		t.Errorf("the key should be reported as redacted, not dropped:\n%s", blob)
	}
}

func TestOversizedManifestIsReported(t *testing.T) {
	big := make([]byte, maxManifestBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	f, ok := Parse("Dockerfile", big)
	if !ok || !hasProblem(f, ReasonTooLarge) {
		t.Fatalf("an oversized manifest must be reported, not dropped: ok=%v %+v", ok, f)
	}
}

// ---------------------------------------------------------------- helpers

func hasKey(keys []EnvKey, name string) bool {
	_, ok := findKey(keys, name)
	return ok
}

func findKey(keys []EnvKey, name string) (EnvKey, bool) {
	for _, k := range keys {
		if k.Name == name {
			return k, true
		}
	}
	return EnvKey{}, false
}

func hasProblem(f File, reason string) bool {
	for _, p := range f.Problems {
		if p.Reason == reason {
			return true
		}
	}
	return false
}

func unitNames(f File) []string {
	out := make([]string, 0, len(f.Units))
	for _, u := range f.Units {
		out = append(out, u.Type+" "+u.Name)
	}
	return out
}

// TestASourceFileIsNotAContainerBuild is a false-positive regression test.
//
// The `Dockerfile.<variant>` convention is real, so classify matches the prefix
// `dockerfile.` — and that matched this package's own `dockerfile.go`. Running
// `cortex infra` on the RepoCortex repository reported that Go file as a
// container build with two stages, `stage1` and `stage2`, at lines 83 and 85.
// All of it invented from Go source.
//
// A fabricated fact is worse than a missing one because the reader cannot tell
// them apart, and a deployment inventory is the worst place for one: nobody
// cross-checks a Dockerfile they did not know they had.
func TestASourceFileIsNotAContainerBuild(t *testing.T) {
	notDockerfiles := []string{
		"internal/analyze/infra/dockerfile.go",
		"dockerfile.go",
		"Dockerfile.ts",
		"pkg/build/dockerfile.py",
		"dockerfile.test",
		"dockerfile.md",
		"DOCKERFILE.JSON",
	}
	for _, p := range notDockerfiles {
		if got := classify(p); got == classDockerfile {
			t.Errorf("%s was classified as a Dockerfile; it is source code, and every "+
				"stage, port and env key read from it would be fabricated", p)
		}
	}

	// The convention itself must keep working — this is a narrowing, not a
	// removal, and losing real multi-variant Dockerfiles would be a worse bug.
	realDockerfiles := []string{
		"Dockerfile",
		"dockerfile",
		"Containerfile",
		"Dockerfile.prod",
		"Dockerfile.alpine",
		"deploy/Dockerfile.staging",
		"services/api/api.dockerfile",
	}
	for _, p := range realDockerfiles {
		if got := classify(p); got != classDockerfile {
			t.Errorf("%s was NOT classified as a Dockerfile (got %v)", p, got)
		}
	}
}
