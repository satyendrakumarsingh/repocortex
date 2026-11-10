package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/infra"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// Two fuzz targets, because the two properties need opposite inputs.
//
// FuzzInfraPersistenceValue fixes the document's shape and fuzzes the KEY, so
// the sentinel can only ever sit to the right of a delimiter. That constraint
// is the whole test: a target that fuzzes the raw manifest can put the sentinel
// in NAME position, where it legitimately is a key name, and the failure is the
// test's rather than the code's. (It did, on the first run.)
//
// FuzzInfraPersistenceShape fuzzes the raw manifest and asserts the structural
// properties instead — determinism, and that no row points at nothing.

var infraShapeSeeds = []string{
	"FROM alpine\nENV DB_PASSWORD=plain-value\nEXPOSE 8080\n",
	"FROM golang AS build\nARG TOKEN=plain-value\nFROM alpine\nEXPOSE 80/udp\n",
	`services:
  api:
    build: ./services/api
    image: example/api:1
    ports: ["8080:80", "${PORT}:90"]
    environment:
      DB_PASSWORD: plain-value
      PLAIN: plain-value
    depends_on: [db]
  db:
    image: postgres
    environment: ["POSTGRES_PASSWORD=plain-value"]
`,
	`apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  template:
    spec:
      containers:
        - name: api
          image: example/api
          ports:
            - containerPort: 8080
          env:
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef: {name: db-creds, key: password}
            - name: PLAIN
              value: plain-value
          resources:
            limits: {memory: 512Mi}
          envFrom:
            - secretRef: {name: extra}
---
apiVersion: v1
kind: Secret
metadata:
  name: db-creds
data:
  password: plain-value
`,
	"services:\n  api:\n    build: ../../outside\n",
	"services:\n  api:\n    build: https://github.com/x/y#main\n",
	"services:\n  api:\n    depends_on: [api]\n",
	"",
	"{{ .Values.x }}\n",
	"\x00\x00",
}

var infraShapePaths = []string{
	"Dockerfile",
	"docker-compose.yml",
	"deploy/docker-compose.yml",
	"k8s/deployment.yaml",
	"charts/api/values.yaml",
	"infra/main.tf",
	"README.md",
}

// FuzzInfraPersistenceShape fuzzes the hop this whole change consists of —
// manifest text in, graph nodes and registry rows out — and asserts the two
// structural properties the index depends on.
//
//  1. THE SHAPING IS DETERMINISTIC. The same manifest shaped twice must produce
//     the same FQNs in the same order, because symbol ids are assigned in that
//     order and the registry digest is a function of the ids every edge
//     references. A map ranged anywhere in this path fails here. It also
//     asserts no two nodes from one manifest share an FQN: two nodes with one
//     name make every lookup by name a coin flip, which is the same class of
//     bug seen from the other end.
//
//  2. NO ROW POINTS AT NOTHING. Every edge names two symbols that were actually
//     built and would survive model.Edge.Validate, and every port cites a line
//     inside the file.
//
// The value property is FuzzInfraPersistenceValue's job; see the head of this
// file for why it cannot be this target's.
func FuzzInfraPersistenceShape(f *testing.F) {
	for _, s := range infraShapeSeeds {
		for i := range infraShapePaths {
			f.Add(uint8(i), s)
		}
	}

	f.Fuzz(func(t *testing.T, which uint8, src string) {
		path := infraShapePaths[int(which)%len(infraShapePaths)]
		file, ok := infra.Parse(path, []byte(src))
		if !ok {
			return
		}

		symbols := infraSymbols(file)

		// Ids are handed out by position, exactly as the sink does it, so the
		// lookup below exercises the same association the real write path uses.
		byFQN := make(map[string]int64, len(symbols))
		for i, s := range symbols {
			byFQN[s.FQN] = int64(i + 1)
		}
		id := func(fqn string) int64 { return byFQN[fqn] }

		units, keySets, problems := infraRows(file, id)
		edges := infraEdges(file, id)
		for _, p := range problems {
			if p.Reason == "" {
				// A problem row with no reason is a row nobody can act on,
				// which is indistinguishable from the silence the table
				// exists to prevent.
				t.Fatalf("problem with no reason: %#v", p)
			}
		}
		for _, ks := range keySets {
			if ks.Name == "" && len(ks.Keys) > 0 {
				t.Fatalf("keys filed under a key set with no name: %#v", ks)
			}
		}

		// --- 1. deterministic shaping ---------------------------------------
		again := infraSymbols(file)
		if len(again) != len(symbols) {
			t.Fatalf("shaping the same manifest twice produced %d then %d symbols",
				len(symbols), len(again))
		}
		for i := range symbols {
			if again[i].FQN != symbols[i].FQN || again[i].Kind != symbols[i].Kind {
				t.Fatalf("symbol %d differs between two shapings: %q/%s vs %q/%s",
					i, symbols[i].FQN, symbols[i].Kind, again[i].FQN, again[i].Kind)
			}
		}
		seen := map[string]struct{}{}
		for _, s := range symbols {
			if _, dup := seen[s.FQN]; dup {
				// Two nodes with one FQN make every lookup by name a coin
				// flip, which is the determinism failure this file guards.
				t.Fatalf("duplicate FQN %q from one manifest", s.FQN)
			}
			seen[s.FQN] = struct{}{}
			if s.Kind != infra.KindUnit && s.Kind != model.KindConfiguration {
				t.Fatalf("unexpected node kind %q", s.Kind)
			}
			if s.FQN == "" || s.Name == "" {
				t.Fatalf("node with no identity: %#v", s)
			}
		}

		// --- 2. nothing points at nothing -----------------------------------
		lines := strings.Count(src, "\n") + 1
		for _, e := range edges {
			if e.SrcID <= 0 || e.SrcID > int64(len(symbols)) ||
				e.DstID <= 0 || e.DstID > int64(len(symbols)) {
				t.Fatalf("edge %s references a symbol that was not built: %d -> %d of %d",
					e.Kind, e.SrcID, e.DstID, len(symbols))
			}
			if err := e.Validate(); err != nil {
				t.Fatalf("edge would be rejected by the registry: %v", err)
			}
			if e.Evidence.Range.Start.Line < 0 || e.Evidence.Range.Start.Line > lines {
				t.Fatalf("edge cites line %d, outside 0..%d", e.Evidence.Range.Start.Line, lines)
			}
		}
		for _, u := range units {
			for _, p := range u.Ports {
				if p.Line <= 0 || p.Line > lines {
					t.Fatalf("port cites line %d, outside 1..%d: %#v", p.Line, lines, p)
				}
			}
		}
	})
}

// FuzzInfraPersistenceValue is the security property of this layer, stated as
// a property.
//
// The KEY is fuzzed and the VALUE is a fixed sentinel, planted to the right of
// the delimiter in every format — the same construction
// infra.FuzzParseNeverEmitsValue uses one layer down, and it has to be repeated
// here rather than trusted: the extractor proving it never RETURNS a value says
// nothing about what the layer that PERSISTS its output does with what it got.
// That distinction is not hypothetical. This target's first run caught a real
// leak: infra.Problem.Detail carries the YAML parser's own message, go-yaml
// quotes the text it failed on, and a planted value came straight back inside
// "unknown anchor '<value>' referenced". The field is no longer persisted.
//
// Everything the write path would hand the registry is serialised and searched:
// symbols, unit rows, ports, keys, key sets, envFrom references, limits,
// problems and edges. A value that reaches any of them reaches disk.
func FuzzInfraPersistenceValue(f *testing.F) {
	for _, seed := range []string{
		"DB_PASSWORD", "a", "", "....", "x-y_z", "AKIAIOSFODNN7EXAMPLE",
		strings.Repeat("k", 300), "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		f.Add(seed)
	}

	const sentinel = "hunter2-super-secret-sentinel"

	f.Fuzz(func(t *testing.T, rawKey string) {
		key := infraKeyish(rawKey)
		if key == "" {
			return
		}

		for _, c := range []struct{ path, src string }{
			{"Dockerfile", "FROM alpine\nENV " + key + "=" + sentinel + "\nEXPOSE 8080\n"},
			{"docker-compose.yml", "services:\n  api:\n    image: a\n    build: ./api\n" +
				"    environment:\n      " + key + ": " + sentinel + "\n"},
			{"docker-compose.yml", "services:\n  api:\n    image: a\n    environment:\n      - " +
				key + "=" + sentinel + "\n"},
			{"k8s/d.yaml", "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n" +
				"    - name: c\n      image: i\n      ports:\n        - containerPort: 80\n" +
				"      env:\n        - name: " + key + "\n          value: " + sentinel + "\n"},
			{"k8s/s.yaml", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\ndata:\n  " +
				key + ": " + sentinel + "\n"},
			{"charts/api/values.yaml", key + ": " + sentinel + "\n"},
			{"infra/main.tf", "resource \"aws_db_instance\" \"main\" {\n  " + key + " = \"" + sentinel + "\"\n}\n"},
		} {
			file, ok := infra.Parse(c.path, []byte(c.src))
			if !ok {
				continue
			}
			symbols := infraSymbols(file)
			byFQN := make(map[string]int64, len(symbols))
			for i, s := range symbols {
				byFQN[s.FQN] = int64(i + 1)
			}
			id := func(fqn string) int64 { return byFQN[fqn] }
			units, keySets, problems := infraRows(file, id)
			edges := infraEdges(file, id)

			blob, err := json.Marshal(struct {
				Symbols  []model.Symbol
				Units    []registry.InfraUnitRecord
				KeySets  []registry.InfraKeySetRecord
				Problems []registry.InfraProblem
				Edges    []model.Edge
			}{symbols, units, keySets, problems, edges})
			if err != nil {
				t.Fatalf("%s: marshal: %v", c.path, err)
			}
			if strings.Contains(string(blob), sentinel) {
				t.Fatalf("%s WOULD PERSIST THE VALUE, with key %q:\n%s", c.path, key, blob)
			}
		}
	})
}

// infraKeyish reduces a fuzzed string to something that can only ever be a key:
// no newline, quote, colon, brace or backslash, so the document's shape is
// fixed and only the name varies. Without it the fuzzer moves the sentinel to
// the LEFT of a delimiter, where it legitimately is a key name.
func infraKeyish(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '_', r == '-', r == '.':
			b.WriteRune(r)
		}
		if b.Len() >= 300 {
			break
		}
	}
	return b.String()
}
