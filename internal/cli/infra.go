package cli

import (
	stdctx "context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/acquire/fs"
	"github.com/satyendrakumarsingh/repocortex/internal/analyze/infra"
)

// InfraMind: what the code RUNS AS, as opposed to what it does.
//
// This report reads the INDEX, like every other report in this CLI. It did not
// always: deployment manifests were extracted but never persisted, so `infra`
// scanned the repository from disk on every run and printed "NOT INDEXED" at
// the bottom of its own output — honest, and useless to everything else, since
// no MCP tool, no `cortex context` and no graph query reads the filesystem.
//
// The disk path is KEPT, behind --disk, and the justification is narrow: this
// command is most useful on a repository someone has just cloned and is about
// to change, which is precisely the moment there is no index. Falling back to
// it silently would be wrong — a stale index and a fresh disk scan disagree,
// and a reader must know which one they are looking at — so it is a flag, and
// the no-index error names it.
//
// What the index adds is not the inventory. It is the JOINS: --for asks which
// service a file runs in, --key asks what changing a configuration key
// restarts, and neither question can be answered by reading manifests alone,
// because both need the manifest facts to sit in the same graph as the code.

// maxScanFiles bounds one scan. A candidate is any YAML file in the repository,
// because a Kubernetes manifest is only identifiable by its contents, and a
// monorepo full of unrelated YAML must not turn this into an unbounded read.
const maxScanFiles = 20000

func newInfraCmd(g *Global) *cobra.Command {
	var (
		showKeys bool
		format   string
		fromDisk bool
		forPath  string
		forKey   string
	)

	cmd := &cobra.Command{
		Use:   "infra",
		Short: "Report the services, ports and configuration a deployment declares",
		Long: `Reads the files that say how this code is DEPLOYED: Dockerfiles, Compose
files, Kubernetes manifests, Helm charts and Terraform.

Source code cannot answer "which service does this code run in". No line of it
says so. That fact lives in a compose file's build context, in a Deployment's
image, and nowhere else — which is why a change-safety tool that reads only
source can tell you a function's callers and still not tell you what restarts
when you change it.

VALUES ARE NEVER PRINTED. These files hold credentials, and a tool that renders
a codebase's secrets into an agent's context window is a security incident
rather than a feature. The guarantee is structural, not a filter: the extractor
has no field that a value fits in, and a Kubernetes Secret is read as the list
of key NAMES it declares. What you get is "DB_PASSWORD, from Secret db-creds",
which is the half you can act on.

WHAT COULD NOT BE READ IS REPORTED. "No Kubernetes manifests found" and "your
Kubernetes manifests could not be parsed" have opposite consequences, so a
manifest that fails to parse is listed with the reason. Helm templates are
reported as unrendered rather than guessed at: rendering one needs Helm's engine
and a values file, and a half-rendered manifest invents services that do not
exist.

Terraform is read as block headers — resource, data, module, provider, variable
— and never as attribute values. That is a coverage limit and it is also why a
password sitting in a .tf file has no path into this output.

INDEXED. These facts are stored by 'cortex index': a service is a node in the
same graph as the code, a configuration key name is the same node the code that
reads it binds to, and 'cortex resolve' joins a service to the endpoints built
from its build context. Two questions follow from that and neither is answerable
from manifests alone:

  --for <path>   which service does this file run in
  --key <NAME>   what does changing this configuration key restart

Use --disk to scan the repository directly instead. That is the right choice on
a repository you have just cloned and have not indexed; it is not a fallback,
because a disk scan and an index can disagree and you should know which you are
reading.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case forPath != "":
				return runInfraForPath(cmd, g, forPath)
			case forKey != "":
				return runInfraForKey(cmd, g, forKey)
			}
			if fromDisk {
				files, scanned, err := scanInfra(cmd.Context(), g)
				if err != nil {
					return err
				}
				if format != "" {
					files = filterFormat(files, format)
				}
				return renderInfra(cmd, g, files, scanned, showKeys, true)
			}
			files, err := indexedInfra(cmd.Context(), g)
			if err != nil {
				return err
			}
			if format != "" {
				files = filterFormat(files, format)
			}
			return renderInfra(cmd, g, files, len(files), showKeys, false)
		},
	}

	cmd.Flags().BoolVar(&showKeys, "keys", false,
		"list every configuration key name each unit sets (names only; values are never read)")
	cmd.Flags().StringVar(&format, "format", "",
		"restrict the report to one format: dockerfile, compose, kubernetes, helm, terraform")
	cmd.Flags().BoolVar(&fromDisk, "disk", false,
		"scan manifests from disk instead of reading the index (works without 'cortex index')")
	cmd.Flags().StringVar(&forPath, "for", "",
		"which deployment unit does this file run in (repo-relative path)")
	cmd.Flags().StringVar(&forKey, "key", "",
		"what does changing this configuration key name restart (names only; values are never read)")
	return cmd
}

// scanInfra walks the repository and parses every manifest it finds.
//
// The walk is the indexer's own (internal/acquire/fs), so this honours
// .gitignore, the configured exclude globs and the file-size ceiling — a report
// that disagreed with the index about which files exist would be worse than no
// report. It returns the number of files considered alongside the results,
// because "we looked at 4,000 files and found no manifests" and "we found no
// files" are different answers.
func scanInfra(ctx stdctx.Context, g *Global) (files []infra.File, scanned int, err error) {
	cfg := g.Config()
	w, err := fs.NewWalker(cfg.RepoRoot, cfg)
	if err != nil {
		return nil, 0, fmt.Errorf("infra: %w", err)
	}
	found, err := w.Walk(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("infra: walk %s: %w", cfg.RepoRoot, err)
	}

	for _, d := range found {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if !infra.Candidate(d.Path) {
			continue
		}
		scanned++
		if scanned > maxScanFiles {
			files = append(files, infra.File{
				Path: d.Path,
				Problems: []infra.Problem{{
					Reason: infra.ReasonLimit,
					Detail: "scan stopped after " + strconv.Itoa(maxScanFiles) + " candidate files",
				}},
			})
			break
		}

		if d.Skipped {
			// A manifest the walk refused is exactly the case this report must
			// not swallow: it looks like "no manifests here" and it is not.
			files = append(files, infra.File{
				Path:     d.Path,
				Problems: []infra.Problem{{Reason: d.SkipReason}},
			})
			continue
		}

		full := filepath.Join(w.Root(), filepath.FromSlash(d.Path))
		src, readErr := os.ReadFile(full) //nolint:gosec // path came from the walker, which is rooted
		if readErr != nil {
			files = append(files, infra.File{
				Path:     d.Path,
				Problems: []infra.Problem{{Reason: "unreadable", Detail: readErr.Error()}},
			})
			continue
		}
		f, ok := infra.Parse(d.Path, src)
		if !ok {
			continue // a YAML file that is not a manifest: not a finding
		}
		files = append(files, f)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, scanned, nil
}

func filterFormat(files []infra.File, format string) []infra.File {
	want := infra.Format(strings.ToLower(strings.TrimSpace(format)))
	out := files[:0:0]
	for _, f := range files {
		if f.Format == want {
			out = append(out, f)
		}
	}
	return out
}

func renderInfra(cmd *cobra.Command, g *Global, files []infra.File, scanned int, showKeys, fromDisk bool) error {
	summary := infra.Summarize(files)
	out := cmd.OutOrStdout()

	if g.JSON {
		return emitJSON(out, map[string]any{
			"summary": summary,
			"files":   files,
			// Which of the two sources produced this, in the machine-readable
			// output as well as the human one. A caller that cannot tell an
			// indexed answer from a disk scan cannot tell a stale answer from
			// a current one.
			"source": infraSource(fromDisk),
		})
	}

	if len(files) == 0 {
		if fromDisk {
			fmt.Fprintf(out, "No deployment manifests found (%d candidate file(s) examined).\n", scanned)
		} else {
			// "The index holds none" and "we did not look" are different
			// statements, and the fix differs: re-index, versus accept that
			// this repository declares no deployment.
			fmt.Fprintln(out, "The index holds no deployment manifests. "+
				"If you expected some, re-run 'cortex index'; to check the "+
				"working tree directly, run 'cortex infra --disk'.")
		}
		fmt.Fprintln(out, "\nRead: Dockerfile, docker-compose.yml, Kubernetes manifests, "+
			"Chart.yaml and values.yaml, and .tf files. CloudFormation, systemd units and "+
			"CI definitions are not read — that is a coverage limit, not a finding about "+
			"your deployment.")
		return nil
	}

	fmt.Fprintf(out, "%d manifest(s), %d unit(s), %d configuration key(s) (%d sensitive by name or source).\n\n",
		summary.Files, summary.Units, summary.EnvKeys, summary.SecretKeys)

	renderProblems(out, files, summary)
	renderUnresolved(out, summary)

	for _, f := range files {
		if len(f.Units) == 0 && len(f.KeySets) == 0 {
			continue // already covered by the problem section
		}
		fmt.Fprintf(out, "%s  [%s]\n", f.Path, f.Format)
		tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		for _, u := range f.Units {
			renderUnit(tw, u)
		}
		for _, ks := range f.KeySets {
			label := ks.Type
			if ks.Secret {
				label += " (sensitive)"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%d key(s)\tline %d\n", ks.Name, label, len(ks.Keys), ks.Line)
		}
		_ = tw.Flush()

		if showKeys {
			renderKeys(out, f)
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintln(out, "No configuration VALUE was read to produce this report: key names, "+
		"their source and their location only.")
	if fromDisk {
		fmt.Fprintln(out, "Read from disk just now (--disk). These rows are NOT the index; "+
			"run 'cortex index' and drop --disk to query them against symbols.")
	} else {
		fmt.Fprintln(out, "Read from the index. Try --for <path> for the service a file runs in, "+
			"or --key <NAME> for what changing a key restarts.")
	}
	return nil
}

// infraSource names which of the two paths produced a report.
func infraSource(fromDisk bool) string {
	if fromDisk {
		return "disk scan at report time"
	}
	return "index"
}

func renderUnit(tw *tabwriter.Writer, u infra.Unit) {
	var detail []string
	if u.Image != "" {
		detail = append(detail, u.Image)
	}
	if p := portSummary(u.Ports); p != "" {
		detail = append(detail, "ports "+p)
	}
	if len(u.Env) > 0 {
		secret := 0
		for _, k := range u.Env {
			if k.Secret {
				secret++
			}
		}
		s := fmt.Sprintf("env %d", len(u.Env))
		if secret > 0 {
			s += fmt.Sprintf(" (%d sensitive)", secret)
		}
		detail = append(detail, s)
	}
	for _, ref := range u.EnvFrom {
		detail = append(detail, "envFrom "+ref.Kind+" "+ref.Name)
	}
	for _, f := range u.EnvFiles {
		detail = append(detail, "env_file "+f)
	}
	if u.BuildContext != "" {
		// The one link from a service back to source. Called out rather than
		// buried, because it is the answer to the question in Long.
		detail = append(detail, "built from "+u.BuildContext)
	}
	if len(u.DependsOn) > 0 {
		detail = append(detail, "depends on "+strings.Join(u.DependsOn, ", "))
	}
	if len(u.Limits) > 0 {
		detail = append(detail, "limits "+limitSummary(u.Limits))
	}
	fmt.Fprintf(tw, "  %s\t%s\t%s\tline %d\n", u.Name, u.Type, strings.Join(detail, "  "), u.Line)
}

// renderKeys lists key NAMES. There is no code path here that could print a
// value: infra.EnvKey has no field holding one.
func renderKeys(out io.Writer, f infra.File) {
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, u := range f.Units {
		for _, k := range u.Env {
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\tline %d\n",
				u.Name, k.Name, k.From, refOrBlank(k), k.Line)
		}
	}
	for _, ks := range f.KeySets {
		for _, k := range ks.Keys {
			fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\tline %d\n",
				ks.Name, k.Name, k.From, sensitivity(k.Secret), k.Line)
		}
	}
	_ = tw.Flush()
}

func refOrBlank(k infra.EnvKey) string {
	if k.Ref != "" {
		return k.Ref
	}
	return sensitivity(k.Secret)
}

// renderProblems leads with what could not be read, because a reader who skims
// the inventory and stops has to have seen it.
func renderProblems(out io.Writer, files []infra.File, s infra.Summary) {
	if s.Problems == 0 {
		return
	}
	fmt.Fprintf(out, "COULD NOT READ (%d) — these files exist and were not understood; "+
		"treat the inventory below as incomplete\n", s.Problems)
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, f := range files {
		for _, p := range f.Problems {
			where := f.Path
			if p.Line > 0 {
				where += ":" + strconv.Itoa(p.Line)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", where, p.Reason, p.Detail)
		}
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
}

// renderUnresolved reports configuration a workload consumes that no manifest
// here declares. Phrased as a prompt, not a verdict: a Secret created by an
// operator or a sealed-secrets controller is the normal case, and the useful
// statement is that those keys are invisible from this repository.
func renderUnresolved(out io.Writer, s infra.Summary) {
	if len(s.Unresolved) == 0 {
		return
	}
	fmt.Fprintf(out, "CONSUMED BUT NOT DECLARED HERE (%d) — a whole Secret or ConfigMap is "+
		"injected from outside this repository, so its keys are not visible in any file\n",
		len(s.Unresolved))
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, u := range s.Unresolved {
		fmt.Fprintf(tw, "  %s %s\tconsumed by %s\t%s:%d\n", u.Kind, u.Name, u.Unit, u.Path, u.Line)
	}
	_ = tw.Flush()
	fmt.Fprintln(out)
}

func portSummary(ports []infra.Port) string {
	if len(ports) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		switch {
		case p.Raw != "":
			parts = append(parts, p.Raw)
		case p.Published > 0 && p.Container > 0:
			parts = append(parts, strconv.Itoa(p.Published)+"->"+strconv.Itoa(p.Container))
		case p.Published > 0:
			parts = append(parts, strconv.Itoa(p.Published))
		case p.Container > 0:
			parts = append(parts, strconv.Itoa(p.Container))
		default:
			continue
		}
		if p.Protocol == "udp" {
			parts[len(parts)-1] += "/udp"
		}
		if len(parts) >= 8 {
			parts = append(parts, "…")
			break
		}
	}
	return strings.Join(parts, ",")
}

func limitSummary(limits []infra.Limit) string {
	parts := make([]string, 0, len(limits))
	for _, l := range limits {
		switch {
		case l.Limit != "" && l.Request != "":
			parts = append(parts, l.Resource+" "+l.Request+"/"+l.Limit)
		case l.Limit != "":
			parts = append(parts, l.Resource+" "+l.Limit)
		case l.Request != "":
			parts = append(parts, l.Resource+" "+l.Request)
		}
		if len(parts) >= 6 {
			break
		}
	}
	return strings.Join(parts, " ")
}

// indexedInfra reads the deployment facts the index holds.
//
// The conversion back into infra.File is deliberate and is the reason the
// registry does not return that type itself. internal/registry is the
// repository adapter; it must not import an analyzer to shape its results
// (CLAUDE.md §1, which records the ImportMatcher this rule already produced
// once). So it returns its own rows and this command — which imports both —
// adapts them, which also means the two report paths render through ONE
// renderer and cannot drift into two different truths about one repository.
//
// One field does not survive the round trip: a unit's command line, which is
// not persisted at all. See migration 0009 for why.
func indexedInfra(ctx stdctx.Context, g *Global) ([]infra.File, error) {
	sess, err := openQuery(ctx, g)
	if err != nil {
		return nil, fmt.Errorf("%w (or run 'cortex infra --disk' to scan the working tree)", err)
	}
	defer sess.close()

	rows, err := sess.reg.InfraReport(ctx, sess.runID)
	if err != nil {
		return nil, err
	}

	out := make([]infra.File, 0, len(rows))
	for _, r := range rows {
		f := infra.File{Path: r.Path, Format: infra.Format(r.Format)}
		for _, u := range r.Units {
			unit := infra.Unit{
				Name: u.Name, Type: u.Type, Line: u.Line, Image: u.Image,
				BuildContext: u.BuildContext, Dockerfile: u.Dockerfile, Final: u.Final,
			}
			for _, p := range u.Ports {
				unit.Ports = append(unit.Ports, infra.Port{
					Container: p.Container, Published: p.Published,
					Protocol: p.Protocol, Name: p.Name, Raw: p.Raw, Line: p.Line,
				})
			}
			for _, k := range u.Keys {
				unit.Env = append(unit.Env, infra.EnvKey{
					Name: k.Name, Line: k.Line, From: infra.EnvSource(k.From),
					Ref: k.Ref, Secret: k.Secret,
				})
			}
			for _, e := range u.EnvFrom {
				unit.EnvFrom = append(unit.EnvFrom, infra.SourceRef{
					Kind: e.Kind, Name: e.Name, Prefix: e.Prefix,
					Optional: e.Optional, Line: e.Line,
				})
			}
			for _, l := range u.Limits {
				unit.Limits = append(unit.Limits, infra.Limit{
					Resource: l.Resource, Request: l.Request, Limit: l.Limit,
				})
			}
			f.Units = append(f.Units, unit)
		}
		for _, ks := range r.KeySets {
			set := infra.KeySet{Name: ks.Name, Type: ks.Type, Line: ks.Line}
			for _, k := range ks.Keys {
				set.Keys = append(set.Keys, infra.EnvKey{
					Name: k.Name, Line: k.Line, From: infra.EnvSource(k.From),
					Ref: k.Ref, Secret: k.Secret,
				})
				if k.Secret {
					set.Secret = true
				}
			}
			f.KeySets = append(f.KeySets, set)
		}
		for _, p := range r.Problems {
			// No Detail: the index does not store the parser's message, for
			// the reason migration 0009 records. --disk still prints it.
			f.Problems = append(f.Problems, infra.Problem{
				Reason: p.Reason, Line: p.Line,
			})
		}
		out = append(out, f)
	}
	return out, nil
}

// runInfraForPath answers "which service does this code run in".
//
// The question no line of source can answer, and the one this whole persistence
// hop exists for. The answer comes from a build context — the only statement in
// a repository that ties a running service to a directory of code — so it is
// reported WITH that context and with the manifest line that declared it, and
// never as a bare service name a reader would have to take on trust.
func runInfraForPath(cmd *cobra.Command, g *Global, target string) error {
	ctx := cmd.Context()
	sess, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer sess.close()

	rel := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(target), "./"))
	units, err := sess.reg.UnitsForPath(ctx, sess.runID, rel)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if g.JSON {
		return emitJSON(out, map[string]any{"path": rel, "units": units})
	}
	if len(units) == 0 {
		fmt.Fprintf(out, "No deployment unit in this index is built from a directory containing %s.\n", rel)
		// Stated rather than left as an empty result, because the two causes
		// need different actions and an empty list looks the same either way.
		fmt.Fprintln(out, "\nEither no manifest here names a build context (pure Kubernetes, "+
			"images built in CI), or this file is outside every context that is named. "+
			"'cortex infra' lists what was found.")
		return nil
	}
	if units[0].Ambiguous {
		fmt.Fprintf(out, "%s is built into %d units from the same directory — "+
			"all of them are true, and nothing in the manifests picks one:\n\n", rel, len(units))
	} else {
		fmt.Fprintf(out, "%s runs in:\n\n", rel)
	}
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, u := range units {
		image := u.Image
		if image == "" {
			image = "(built from source)"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\tbuild context %s\t%s:%d\n",
			u.Name, u.Type, image, u.BuildContext, u.Manifest, u.Line)
	}
	return tw.Flush()
}

// runInfraForKey answers "what does changing this configuration key restart".
//
// Restarting is the consequence people care about and the one the graph can
// now state: a unit that SETS a key is a unit whose pods roll when the key
// changes. The key's VALUE is not read, printed or stored — this is a report
// about which workloads name it.
func runInfraForKey(cmd *cobra.Command, g *Global, key string) error {
	ctx := cmd.Context()
	sess, err := openQuery(ctx, g)
	if err != nil {
		return err
	}
	defer sess.close()

	impact, err := sess.reg.UnitsForConfigKey(ctx, sess.runID, key)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	if g.JSON {
		return emitJSON(out, impact)
	}
	if len(impact.Units) == 0 {
		fmt.Fprintf(out, "No indexed deployment unit sets %s.\n", key)
		fmt.Fprintln(out, "\nThat does not mean nothing reads it: a key supplied by a Secret "+
			"created outside this repository is invisible here. 'cortex config' reports "+
			"the keys code reads.")
		return nil
	}
	fmt.Fprintf(out, "Changing %s restarts %d unit(s):\n\n", key, len(impact.Units))
	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, u := range impact.Units {
		fmt.Fprintf(tw, "  %s\t%s\t%s:%d\n", u.Name, u.Type, u.Manifest, u.Line)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(impact.Sources) > 0 {
		fmt.Fprintf(out, "\nSupplied as: %s\n", strings.Join(impact.Sources, ", "))
	}
	if impact.Secret {
		fmt.Fprintln(out, "This key is a credential by name or by source. Its VALUE was never "+
			"read and is not stored in the index.")
	}
	return nil
}
