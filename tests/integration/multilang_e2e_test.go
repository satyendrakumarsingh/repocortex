package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/capability"
	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/graph"
	"github.com/satyendrakumarsingh/repocortex/internal/pipeline"
	"github.com/satyendrakumarsingh/repocortex/internal/prune"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
	"github.com/satyendrakumarsingh/repocortex/internal/store"
	"github.com/satyendrakumarsingh/repocortex/internal/web"
)

// TestMultiLanguageEndToEnd verifies that RepoCortex indexes, resolves, queries,
// prunes, and serves every core supported language end-to-end.
//
// Covers all 12 core languages (1 typed + 11 resolved):
//  1. Go
//  2. Python
//  3. TypeScript
//  4. TSX
//  5. JavaScript
//  6. Java
//  7. Rust
//  8. C#
//  9. Kotlin
//  10. PHP
//  11. Ruby
//  12. Scala
//
// plus representative parsed languages (SQL, Shell).
func TestMultiLanguageEndToEnd(t *testing.T) {
	ctx := context.Background()

	// 1. Create a temporary polyglot workspace
	dir := t.TempDir()

	files := map[string]string{
		// Go
		"pkg/calc/calc.go": `package calc

type Calculator struct{}

func (c *Calculator) Add(a, b int) int {
	return a + b
}
`,
		"pkg/app/main.go": `package app

import "demo/pkg/calc"

func Run() int {
	c := &calc.Calculator{}
	return c.Add(1, 2)
}
`,

		// Python
		"pyapp/math_ops.py": `class MathEngine:
    def compute(self, x: int) -> int:
        return x * 2
`,
		"pyapp/main.py": `from pyapp.math_ops import MathEngine

def execute():
    m = MathEngine()
    return m.compute(10)
`,

		// TypeScript
		"ts/service.ts": `export class OrderService {
    processOrder(id: string): boolean {
        return id.length > 0;
    }
}
`,
		"ts/controller.ts": `import { OrderService } from "./service";

export class OrderController {
    private svc = new OrderService();
    handle(id: string) {
        return this.svc.processOrder(id);
    }
}
`,

		// TSX
		"tsx/Widget.tsx": `import React from 'react';

export interface WidgetProps {
    title: string;
}

export const Widget: React.FC<WidgetProps> = ({ title }) => {
    return <div className="widget">{title}</div>;
};
`,
		"tsx/App.tsx": `import React from 'react';
import { Widget } from './Widget';

export const App = () => {
    return <Widget title="RepoCortex Dashboard" />;
};
`,

		// JavaScript
		"js/utils.js": `class Formatter {
    format(val) {
        return "val: " + val;
    }
}
module.exports = { Formatter };
`,
		"js/index.js": `const { Formatter } = require("./utils");

function run() {
    const f = new Formatter();
    return f.format("test");
}
module.exports = { run };
`,

		// Java
		"src/main/java/com/demo/Worker.java": `package com.demo;

public class Worker {
    public void doWork() {
        System.out.println("working");
    }
}
`,
		"src/main/java/com/demo/App.java": `package com.demo;

import com.demo.Worker;

public class App {
    public static void main(String[] args) {
        Worker w = new Worker();
        w.doWork();
    }
}
`,

		// Rust
		"rust/engine.rs": `pub struct Processor;

impl Processor {
    pub fn process(&self, data: &str) -> usize {
        data.len()
    }
}
`,
		"rust/main.rs": `mod engine;
use engine::Processor;

pub fn start() {
    let p = Processor;
    p.process("hello");
}
`,

		// C#
		"dotnet/Logger.cs": `namespace Demo.Services {
    public class Logger {
        public void Log(string msg) {}
    }
}
`,
		"dotnet/Program.cs": `using System;
using Demo.Services;

namespace Demo {
    public class Program {
        public static void Main() {
            var l = new Logger();
            l.Log("started");
        }
    }
}
`,

		// Kotlin
		"kt/Helper.kt": `package demo.util

class StringHelper {
    fun transform(s: String): String = s.uppercase()
}
`,
		"kt/Runner.kt": `package demo.util

import demo.util.StringHelper

class Runner {
    fun run() {
        val h = StringHelper()
        h.transform("test")
    }
}
`,

		// PHP
		"php/Database.php": `<?php
namespace Demo;

class Database {
    public function query($sql) {
        return [];
    }
}
`,
		"php/Repository.php": `<?php
namespace Demo;

use Demo\Database;

class Repository {
    private $db;
    public function __construct(Database $db) {
        $this->db = $db;
    }
    public function find() {
        return $this->db->query("SELECT 1");
    }
}
`,

		// Ruby
		"ruby/lib/payment.rb": `module Payment
  class Gateway
    def charge(amount)
      amount > 0
    end
  end
end
`,
		"ruby/lib/checkout.rb": `require_relative './payment'

class Checkout
  def process
    gw = Payment::Gateway.new
    gw.charge(100)
  end
end
`,

		// Scala
		"scala/Pipeline.scala": `package demo.pipeline

trait Stage {
  def execute(): Unit
}

class Pipeline(stage: Stage) {
  def run(): Unit = stage.execute()
}
`,

		// Parsed SQL & Shell
		"db/schema.sql": `CREATE TABLE accounts (
    id VARCHAR(64) PRIMARY KEY,
    balance BIGINT NOT NULL
);
`,
		"scripts/build.sh": `#!/bin/bash
echo "Building polyglot project..."
`,
	}

	// Write all files
	for relPath, content := range files {
		fullPath := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 2. Initialize RepoCortex store in-memory
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reg := registry.New(db)
	run, err := reg.BeginRun(ctx, "auto", "test-multilang")
	if err != nil {
		t.Fatal(err)
	}

	writer, err := reg.NewWriter(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	sink := pipeline.NewRegistrySink(writer)

	walker, err := fs.NewWalker(dir, config.Config{ProjectName: "test-polyglot"})
	if err != nil {
		t.Fatal(err)
	}

	runner := pipeline.NewRunner(walker, pipeline.Options{
		Workers: 4,
	})

	stats, err := runner.Run(ctx, sink)
	if err != nil {
		t.Fatalf("pipeline index failed: %v", err)
	}

	if stats.FilesIndexed < int64(len(files)) {
		t.Fatalf("indexed %d files, expected at least %d", stats.FilesIndexed, len(files))
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitRun(ctx, run.ID); err != nil {
		t.Fatal(err)
	}

	// 3. Assert symbols exist for EVERY language
	langSymbols := map[string][]string{
		"go":         {"Calculator", "Add", "Run"},
		"python":     {"MathEngine", "compute", "execute"},
		"typescript": {"OrderService", "processOrder", "OrderController", "handle"},
		"tsx":        {"Widget", "App"},
		"javascript": {"Formatter", "format", "run"},
		"java":       {"Worker", "doWork", "App", "main"},
		"rust":       {"Processor", "process", "start"},
		"c_sharp":    {"Logger", "Log", "Program", "Main"},
		"kotlin":     {"StringHelper", "transform", "Runner"},
		"php":        {"Database", "query", "Repository", "find"},
		"ruby":       {"Payment", "Gateway", "charge", "Checkout", "process"},
		"scala":      {"Stage", "Pipeline", "run"},
		"sql":        {"accounts"},
	}

	for lang, expectedSyms := range langSymbols {
		t.Run("symbols_"+lang, func(t *testing.T) {
			for _, sym := range expectedSyms {
				var count int
				err := db.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM symbols s
					 JOIN files f ON f.id = s.file_id
					 WHERE f.language = ? AND s.name = ?`, lang, sym).Scan(&count)
				if err != nil {
					t.Fatalf("query symbol %s (%s): %v", sym, lang, err)
				}
				if count == 0 {
					t.Errorf("language %s: symbol %q was NOT extracted by parser", lang, sym)
				}
			}
		})
	}

	// 4. Run Resolution Pipeline
	resolveReport, err := pipeline.Resolve(ctx, reg, run.ID, pipeline.ResolveOptions{})
	if err != nil {
		t.Fatalf("resolver failed: %v", err)
	}

	t.Logf("Multi-language resolution report: %d edges created", resolveReport.Edges)

	// 5. Invariant: No cross-language impossible edges
	t.Run("no_impossible_cross_language_edges", func(t *testing.T) {
		rows, err := db.QueryContext(ctx, `
			SELECT sf.language, df.language, COUNT(*)
			FROM edges e
			JOIN symbols s ON s.id = e.src_id JOIN files sf ON sf.id = s.file_id
			JOIN symbols d ON d.id = e.dst_id JOIN files df ON df.id = d.file_id
			WHERE e.kind = 'CALLS' AND e.source = 'source_code' AND e.confidence = 'high'
			  AND sf.language != df.language
			GROUP BY 1, 2
		`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()

		allowedCross := map[[2]string]bool{
			{"typescript", "tsx"}:        true,
			{"tsx", "typescript"}:        true,
			{"typescript", "javascript"}: true,
			{"javascript", "typescript"}: true,
			{"java", "kotlin"}:           true,
			{"kotlin", "java"}:           true,
			{"java", "scala"}:            true,
			{"scala", "java"}:            true,
		}

		for rows.Next() {
			var lang1, lang2 string
			var count int
			if err := rows.Scan(&lang1, &lang2, &count); err != nil {
				t.Fatal(err)
			}
			if !allowedCross[[2]string{lang1, lang2}] {
				t.Errorf("impossible cross-language edge: %s -> %s (%d calls)", lang1, lang2, count)
			}
		}
	})

	// 6. Test Pruner across polyglot files (AST Pruning)
	t.Run("ast_pruning_multilang", func(t *testing.T) {
		pruneTests := []struct {
			path     string
			keepSig  string
			stripVal string
		}{
			{"pkg/calc/calc.go", "func (c *Calculator) Add", "return a + b"},
			{"pyapp/math_ops.py", "def compute", "return x * 2"},
			{"ts/service.ts", "processOrder", "return id.length > 0;"},
			{"src/main/java/com/demo/Worker.java", "public void doWork()", "System.out.println"},
			{"rust/engine.rs", "pub fn process", "data.len()"},
		}

		for _, pt := range pruneTests {
			src := files[pt.path]
			pruned, err := prune.PruneFile(pt.path, []byte(src))
			if err != nil {
				t.Errorf("prune %s failed: %v", pt.path, err)
				continue
			}
			prunedStr := string(pruned)
			if len(pruned) >= len(src) && len(src) > 50 {
				t.Errorf("prune %s did not reduce size: was %d, now %d", pt.path, len(src), len(pruned))
			}
			if pt.stripVal != "" && stringContains(prunedStr, pt.stripVal) {
				t.Errorf("prune %s still contains implementation body %q", pt.path, pt.stripVal)
			}
		}
	})

	// 7. Test Web API Server over polyglot repository
	t.Run("web_portal_serves_polyglot", func(t *testing.T) {
		caps := capability.NewDeps(reg, graph.New(db))

		srv, err := web.New(web.Deps{
			Registry: reg,
			Caps:     caps,
			Config: config.Config{
				ProjectName: "test-polyglot",
			},
			IndexPath: filepath.Join(dir, ".cortex", "index.db"),
		})
		if err != nil {
			t.Fatal(err)
		}

		h := srv.Handler()

		endpoints := []string{
			"/api/health",
			"/api/overview",
			"/api/graph",
			"/api/symbols",
			"/api/events",
			"/api/missed",
		}

		for _, ep := range endpoints {
			req := httptest.NewRequest(http.MethodGet, ep, nil)
			req.Host = "127.0.0.1"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("endpoint %s returned status %d: %s", ep, w.Code, w.Body.String())
			}
			if stringContains(w.Body.String(), `"undefined"`) {
				t.Errorf("endpoint %s serialized \"undefined\"", ep)
			}
		}
	})
}

func stringContains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || len(s) > 0 && sqlLike(s, sub)))
}

func sqlLike(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
