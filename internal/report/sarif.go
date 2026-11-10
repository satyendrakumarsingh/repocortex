// Package report turns RepoCortex's analysis results into artefacts other
// systems read: SARIF for code scanning, Markdown for a pull-request comment.
//
// It is pure transformation. It never opens the index and never re-runs an
// analysis; callers hand it the reports the capability packages produced.
package report

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
)

const (
	sarifSchema  = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifVersion = "2.1.0"
	toolName     = "RepoCortex"
	toolURI      = "https://github.com/satyendrakumarsingh/repocortex"
)

// Finding is one result in a tool-neutral shape.
type Finding struct {
	RuleID          string
	RuleDescription string
	// Severity is RepoCortex's own scale: high, medium or low.
	Severity   string
	Message    string
	Path       string
	Line       int
	Confidence string
}

// SARIFLog is the subset of SARIF 2.1.0 that GitHub code scanning reads.
type SARIFLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []SARIFRun `json:"runs"`
}

type SARIFRun struct {
	Tool        SARIFTool         `json:"tool"`
	Invocations []SARIFInvocation `json:"invocations,omitempty"`
	Results     []SARIFResult     `json:"results"`
}

// SARIFInvocation carries run-level notifications.
type SARIFInvocation struct {
	ExecutionSuccessful        bool                `json:"executionSuccessful"`
	ToolExecutionNotifications []SARIFNotification `json:"toolExecutionNotifications,omitempty"`
}

// SARIFNotification is a run-level message that is not tied to a result.
type SARIFNotification struct {
	Level   string    `json:"level"`
	Message SARIFText `json:"message"`
}

type SARIFTool struct {
	Driver SARIFDriver `json:"driver"`
}

type SARIFDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version"`
	InformationURI string      `json:"informationUri"`
	Rules          []SARIFRule `json:"rules"`
}

type SARIFRule struct {
	ID               string    `json:"id"`
	ShortDescription SARIFText `json:"shortDescription"`
}

type SARIFText struct {
	Text string `json:"text"`
}

type SARIFResult struct {
	RuleID     string            `json:"ruleId"`
	Level      string            `json:"level"`
	Message    SARIFText         `json:"message"`
	Locations  []SARIFLocation   `json:"locations,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
}

type SARIFLocation struct {
	PhysicalLocation SARIFPhysical `json:"physicalLocation"`
}

type SARIFPhysical struct {
	ArtifactLocation SARIFArtifact `json:"artifactLocation"`
	Region           *SARIFRegion  `json:"region,omitempty"`
}

type SARIFArtifact struct {
	URI string `json:"uri"`
}

type SARIFRegion struct {
	StartLine int `json:"startLine"`
}

// SARIFOption configures BuildSARIF.
type SARIFOption func(*sarifConfig)

type sarifConfig struct{ pathPrefix string }

// WithPathPrefix prepends prefix to every finding path, for repositories
// analysed from a subdirectory. Joined with forward slashes.
func WithPathPrefix(prefix string) SARIFOption {
	return func(c *sarifConfig) { c.pathPrefix = prefix }
}

// BuildSARIF returns a single-run SARIF log. Findings are sorted so the same
// input always yields the same bytes; the input slice is not modified.
//
// Findings without a path are not emitted as results: code scanning drops or
// rejects results that have no location. They are counted in a run-level
// notification instead, and remain visible in the PR comment.
func BuildSARIF(toolVersion string, findings []Finding, opts ...SARIFOption) SARIFLog {
	var cfg sarifConfig
	for _, o := range opts {
		o(&cfg)
	}
	sorted := append([]Finding(nil), findings...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Message != b.Message {
			return a.Message < b.Message
		}
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		return a.Confidence < b.Confidence
	})

	rules := []SARIFRule{}
	seen := map[string]bool{}
	results := make([]SARIFResult, 0, len(sorted))
	locationless := 0
	for _, f := range sorted {
		if !seen[f.RuleID] {
			seen[f.RuleID] = true
			rules = append(rules, SARIFRule{ID: f.RuleID, ShortDescription: SARIFText{Text: f.RuleDescription}})
		}
		if f.Path == "" {
			locationless++
			continue
		}
		p := f.Path
		if cfg.pathPrefix != "" {
			p = path.Join(cfg.pathPrefix, p)
		}
		uri := (&url.URL{Path: p}).EscapedPath()
		first, _, _ := strings.Cut(uri, "/")
		if strings.Contains(first, ":") {
			uri = "./" + uri // a leading "a:" would parse as a URI scheme
		}
		loc := SARIFLocation{PhysicalLocation: SARIFPhysical{ArtifactLocation: SARIFArtifact{URI: uri}}}
		if f.Line > 0 {
			loc.PhysicalLocation.Region = &SARIFRegion{StartLine: f.Line}
		}
		res := SARIFResult{
			RuleID: f.RuleID, Level: sarifLevel(f.Severity), Message: SARIFText{Text: f.Message},
			Locations: []SARIFLocation{loc},
		}
		if f.Confidence != "" {
			res.Properties = map[string]string{"confidence": f.Confidence}
		}
		results = append(results, res)
	}

	run := SARIFRun{
		Tool:    SARIFTool{Driver: SARIFDriver{Name: toolName, Version: toolVersion, InformationURI: toolURI, Rules: rules}},
		Results: results,
	}
	if locationless > 0 {
		run.Invocations = []SARIFInvocation{{
			ExecutionSuccessful: true,
			ToolExecutionNotifications: []SARIFNotification{{
				Level: "warning",
				Message: SARIFText{Text: fmt.Sprintf("%d findings have no source location and are not shown in code scanning; "+
					"see the RepoCortex PR comment or `cortex missed` / `cortex arch`.", locationless)},
			}},
		}}
	}
	return SARIFLog{Schema: sarifSchema, Version: sarifVersion, Runs: []SARIFRun{run}}
}

// sarifLevel maps RepoCortex severity to a SARIF level. Unknown severities map
// to "note" rather than "error": an unrecognised value must not fail a build.
func sarifLevel(severity string) string {
	switch severity {
	case "high":
		return "error"
	case "medium":
		return "warning"
	default:
		return "note"
	}
}
