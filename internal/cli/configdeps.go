package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/deps"

	"github.com/spf13/cobra"
)

// ConfigMind and DepMind, answered from the shared index.
//
// Both commands lead with the MISMATCH rather than the inventory, because the
// inventory is the boring half. "You declare 47 settings" is a number; "three
// settings your code reads are declared nowhere" is a deployment about to fail
// at start-up.
//
// Neither is phrased as a verdict. A key can be assembled at run time and a
// dependency loaded by reflection, so both reports are prompts to check.

func newConfigCmd(g *Global) *cobra.Command {
	var showAll bool

	cmd := &cobra.Command{
		Use:   "config",
		Short: "Report configuration keys and what reads them",
		Long: `Reports every configuration key the repository declares or reads.

VALUES ARE NEVER READ. Not masked, not truncated — never read at all. The
scanners stop at the delimiter and no structure in this tool has a field to hold
a value, which is a stronger guarantee than redaction: a redaction list can be
wrong about a key nobody anticipated, and a data structure with nowhere to put a
value cannot leak one. Keys whose NAME indicates a credential are labelled
sensitive, which requires reading nothing.

The two findings worth acting on are mismatches. A key read by code that nothing
declares is how a service fails at start-up. A key declared and never read is
either dead configuration or a lookup this scan cannot see.

Declarations are read from .env, YAML, JSON, TOML and .properties files.
Kubernetes, Helm, Terraform and CI definitions are NOT read.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			keys, err := s.reg.ConfigReport(ctx, s.runID)
			if err != nil {
				return err
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"index_revision": s.runID, "keys": keys,
				})
			}

			out := cmd.OutOrStdout()
			if len(keys) == 0 {
				fmt.Fprintln(out, "No configuration keys found.")
				fmt.Fprintln(out, "\nDeclarations are read from .env, YAML, JSON, TOML and "+
					".properties files; usage from os.Getenv, @Value, process.env and similar "+
					"lookups. Kubernetes, Helm, Terraform and CI definitions are not read — "+
					"that is a coverage limit, not a finding about your code.")
				return nil
			}

			var missing, unread, secrets int
			for _, k := range keys {
				switch {
				case !k.Declared:
					missing++
				case k.Readers == 0:
					unread++
				}
				if k.Secret {
					secrets++
				}
			}

			if missing > 0 {
				fmt.Fprintf(out, "READ BUT NOT DECLARED (%d) — a service that needs these and "+
					"finds nothing fails at start-up\n", missing)
				tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
				for _, k := range keys {
					if k.Declared {
						continue
					}
					fmt.Fprintf(tw, "  %s\t%d reader(s)\t%s\n", k.Name, k.Readers, sensitivity(k.Secret))
				}
				_ = tw.Flush()
				fmt.Fprintln(out)
			}

			if !showAll {
				fmt.Fprintf(out, "%d key(s) total: %d declared and never read, %d labelled "+
					"sensitive by name.\n", len(keys), unread, secrets)
				fmt.Fprintln(out, "Pass --all for the full inventory.")
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY\tREADERS\tDECLARED AT\tSENSITIVE")
			for _, k := range keys {
				where := "(not declared)"
				if k.Declared {
					where = fmt.Sprintf("%s:%d", k.FilePath, k.Line)
				}
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", k.Name, k.Readers, where, sensitivity(k.Secret))
			}
			_ = tw.Flush()
			fmt.Fprintf(out, "\n%d key(s). No configuration VALUE was read to produce this report.\n",
				len(keys))
			return nil
		},
	}
	cmd.Flags().BoolVar(&showAll, "all", false, "list every key, not only the mismatches")
	return cmd
}

func sensitivity(secret bool) string {
	if secret {
		return "sensitive (by name)"
	}
	return ""
}

func newDepsCmd(g *Global) *cobra.Command {
	var showAll bool

	cmd := &cobra.Command{
		Use:   "deps",
		Short: "Report declared dependencies and whether anything imports them",
		Long: `Reports what the project declares it depends on, and joins that against
the imports actually recorded in the index.

Manifests, not lock files. A lock file is the resolved transitive closure —
hundreds of packages, most of which no line of this repository mentions. What a
project depends on is a statement its authors made, and "you declare cobra and
never import it" is actionable in a way "your lock file contains golang.org/x/sys"
is not.

NO VULNERABILITY DATA. Spec §27 is explicit that vulnerability information must
not be invented, and a scanner shipping an embedded advisory list is exactly
that — stale the day it is built. Versions are reported accurately so a real
scanner can consume them; the lookup belongs against a live database.

An unused dependency is a PROMPT, not a verdict: a package can be loaded by
reflection, required by a build plugin, or imported under a name that does not
match its distribution name.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, err := openQuery(ctx, g)
			if err != nil {
				return err
			}
			defer s.close()

			// The matcher is supplied here rather than imported by the
			// registry: which import paths belong to a package is ecosystem
			// knowledge, and the storage layer has no business holding it.
			// See registry.ImportMatcher.
			list, err := s.reg.DependencyReport(ctx, s.runID, importMatcher)
			if err != nil {
				return err
			}

			if g.JSON {
				return emitJSON(cmd.OutOrStdout(), map[string]any{
					"index_revision": s.runID, "dependencies": list,
				})
			}

			out := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(out, "No dependency manifest found.")
				fmt.Fprintln(out, "\nRead: go.mod, package.json, pom.xml, build.gradle, "+
					"requirements.txt, pyproject.toml, Cargo.toml.")
				return nil
			}

			var unused []int
			for i, d := range list {
				if d.Importers == 0 && !d.Indirect {
					unused = append(unused, i)
				}
			}

			if len(unused) > 0 {
				fmt.Fprintf(out, "DECLARED BUT NOT IMPORTED (%d)\n", len(unused))
				tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
				for _, i := range unused {
					d := list[i]
					fmt.Fprintf(tw, "  %s\t%s\t%s\t%s:%d\n", d.Name, d.Version, d.Scope, d.Manifest, d.Line)
				}
				_ = tw.Flush()
				fmt.Fprintln(out, "\n  Check before removing: a package can be loaded by reflection, "+
					"required by a build plugin, or imported under a name that differs from its "+
					"distribution name.")
				fmt.Fprintln(out)
			}

			if !showAll {
				fmt.Fprintf(out, "%d dependency(ies) declared. Pass --all for the full list.\n", len(list))
				return nil
			}

			tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
			fmt.Fprintln(tw, "DEPENDENCY\tVERSION\tECOSYSTEM\tSCOPE\tIMPORTED BY")
			for _, d := range list {
				importers := fmt.Sprintf("%d file(s)", d.Importers)
				if d.Importers == 0 {
					importers = "nothing"
					if d.Indirect {
						importers = "nothing (marked indirect)"
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Name, d.Version, d.Ecosystem, d.Scope, importers)
			}
			_ = tw.Flush()
			fmt.Fprintf(out, "\n%d dependency(ies). No vulnerability data is reported — see --help.\n",
				len(list))
			return nil
		},
	}
	cmd.Flags().BoolVar(&showAll, "all", false, "list every dependency, not only the unused ones")
	return cmd
}

// importMatcher adapts the analysis layer's ecosystem rules to the port the
// registry declares.
func importMatcher(name, ecosystem, importPath string) bool {
	return deps.Matches(deps.Dependency{
		Name:      name,
		Ecosystem: deps.Ecosystem(ecosystem),
	}, importPath)
}
