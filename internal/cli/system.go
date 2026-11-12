package cli

import (
	stdctx "context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/satyendrakumarsingh/repocortex/internal/multirepo"
	"github.com/satyendrakumarsingh/repocortex/internal/registry"
)

// newSystemCmd federates a read-only view across several repositories' indexes.
//
// The distinction that justifies the command: every other command in this tool
// answers a question about ONE repository, and the question that matters most in
// a microservice estate is not one of them. "If I change this response type,
// which other repositories break?" needs the frontend's outbound call and the
// backend's route in the same answer, and today those live in two databases that
// know nothing of each other. The facts are already indexed on both sides; the
// only thing missing is the edge between them.
//
// Why it federates rather than building a combined index — the design decision
// everything else follows from, and the one most likely to be "improved" later:
// each member repository is indexed at its own commit on its own schedule, one
// committed run per database is an invariant the two-phase swap depends on, and
// a merged database would have to be rebuilt whenever any member changed. So
// each index stays where it is and is opened read-only, exactly as `diff` does
// with --from-index.
//
// Why the exit code is not always zero: a system graph missing a member reports
// a smaller system than the one that exists, and reads as completeness. When a
// member cannot be read the report is still printed — a partial answer beats
// none — and the command exits 2 so a CI job cannot mistake it for a clean one.
func newSystemCmd(g *Global) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Federate a read-only system graph across several repositories' indexes",
		Long: `Joins several repositories' indexes into one view and reports the edges that
cross between them: an outbound HTTP call in one repository answered by an
endpoint declared in another, and database tables that more than one repository
touches.

Nothing is merged and nothing is migrated. Each member keeps its own index, its
own runs and its own digest, and this opens them read-only and assembles a view
— the same shape 'repocortex diff --from-index' uses, and for the same reasons:
members are indexed at different commits on different schedules, one committed
run per database is an invariant the index swap depends on, and a combined
database would have to be rebuilt whenever any member changed.

Name the members inline, or keep them in a file:

  repocortex system --member web=../web/.repocortex/index.db \
                  --member orders=../orders/.repocortex/index.db

  repocortex system --members ./repocortex.system.yml

The file is the one to use past two repositories. The member list is a property
of the ESTATE rather than of an invocation — it is the same for everyone on the
team, it changes when a service is added, and it deserves review — and retyping
eight flags is how a member silently goes missing, which is the precise failure
this report exists to prevent. It is deliberately not .repocortex.yml: that file
is per-repository indexing configuration, and the estate is not a property of
any one of its members. The format is:

  members:
    - name: web
      index: ../web/.repocortex/index.db
    - name: orders
      index: ../orders/.repocortex/index.db

Paths in the file are resolved relative to the FILE, so the command means the
same thing from every directory.

WHAT THE CONFIDENCE MEANS HERE, because it is weaker than everywhere else in
this tool. Matching '/users/{id}' against '/users/:id' against '/users/%s' is a
guess no compiler proved, so no fact this command produces is ever 'exact', and
an edge's confidence is the MINIMUM over every leg of the path including the
match itself. Each leg is printed under its edge, so the minimum can be checked
rather than believed. A call that two members could both answer is refused
rather than assigned: there is no router spanning two repositories, so those are
two answers, not one.

The report always ends with what it could NOT join — calls matching no known
endpoint, calls matching several, endpoints nobody in the system calls — and it
always begins with the members, including any whose index is absent, uncommitted
or unreadable. A federated view that silently omits a member is worse than no
federated view, because it reads as completeness. Members indexed at different
commits or by different builds are reported for the same reason: a system graph
assembled from a six-week-old index of one service is a picture of a system that
no longer exists, and only the tool can know that.

Exit status is 0 when every member was read, and 2 when the report is a floor
rather than a total because one was not.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSystem(cmd, g)
		},
	}

	cmd.Flags().StringArray("member", nil,
		"a member repository as name=path-to-index.db (repeatable)")
	cmd.Flags().String("members", "",
		"read the member list from this estate file (YAML: members: [{name, index}])")
	cmd.Flags().Int("max-edges", 0, "cap the cross-repository edges reported (0 = default)")
	cmd.Flags().Int("max-unjoined", 0, "cap each unjoined section (0 = default)")
	cmd.Flags().Int("max-shared-tables", 0, "cap the shared-table section (0 = default)")
	cmd.Flags().Duration("stale-after", 0,
		"age at which a member index is called out as old (0 = default)")
	return cmd
}

// systemExitIncomplete is returned when the report was produced but a member
// could not be read. Distinct from 1 (the command failed) because the two need
// different reactions: one means fix the invocation, the other means the answer
// is a floor.
const systemExitIncomplete = 2

func runSystem(cmd *cobra.Command, g *Global) error {
	ctx := cmd.Context()

	entries, err := systemMembers(cmd)
	if err != nil {
		return err
	}

	opened := &memberCatalogue{byPath: map[string]*openIndex{}}
	defer opened.closeAll()

	members := make([]multirepo.Member, 0, len(entries))
	for _, e := range entries {
		members = append(members, opened.member(ctx, e))
	}

	opts := []multirepo.Option{multirepo.WithRunningVersion(Version)}
	if n, _ := cmd.Flags().GetInt("max-edges"); n > 0 {
		opts = append(opts, multirepo.WithMaxEdges(n))
	}
	if n, _ := cmd.Flags().GetInt("max-unjoined"); n > 0 {
		opts = append(opts, multirepo.WithMaxUnjoined(n))
	}
	if n, _ := cmd.Flags().GetInt("max-shared-tables"); n > 0 {
		opts = append(opts, multirepo.WithMaxSharedTables(n))
	}
	if d, _ := cmd.Flags().GetDuration("stale-after"); d > 0 {
		opts = append(opts, multirepo.WithStaleAfter(d))
	}

	fed, err := multirepo.New(opts...)
	if err != nil {
		return fmt.Errorf("system: %w", err)
	}
	rep, err := fed.Build(ctx, members)
	if err != nil {
		return err
	}

	if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
		if err := emitJSON(cmd.OutOrStdout(), rep); err != nil {
			return err
		}
	} else if err := multirepo.Render(cmd.OutOrStdout(), rep); err != nil {
		return fmt.Errorf("system: render report: %w", err)
	}

	if !rep.Complete() {
		// Returned rather than os.Exit'd, so the code is testable. Silent
		// because the report is already on stdout and names the missing members
		// itself; an "error:" line under it would read as a malfunction rather
		// than as the verdict it is.
		return &ExitCodeError{Code: systemExitIncomplete, Silent: true,
			Err: fmt.Errorf("system: %d member(s) could not be read; every count is a floor",
				len(rep.Unjoined.UnavailableMembers))}
	}
	return nil
}

// systemMembers gathers the member list from the estate file and the flags.
//
// Both spellings are allowed together — a team's committed estate plus one
// checkout somebody is trying out is a real case — but a name appearing twice
// is refused rather than resolved. Two members sharing a name would make every
// "which repository breaks" line ambiguous, which is the one property the
// output cannot lose.
func systemMembers(cmd *cobra.Command) ([]multirepo.EstateMember, error) {
	var out []multirepo.EstateMember

	if path, _ := cmd.Flags().GetString("members"); path != "" {
		fromFile, err := multirepo.LoadEstate(path)
		if err != nil {
			return nil, err
		}
		out = append(out, fromFile...)
	}
	raw, _ := cmd.Flags().GetStringArray("member")
	for _, r := range raw {
		m, err := multirepo.ParseMemberFlag(r)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}

	if len(out) < 2 {
		return nil, errors.New(
			"name at least two members: a system graph is the edges BETWEEN repositories, " +
				"and one repository is already answered by 'repocortex api' and 'repocortex db'.\n" +
				"  repocortex system --member web=../web/.repocortex/index.db \\\n" +
				"                  --member orders=../orders/.repocortex/index.db\n" +
				"  repocortex system --members ./repocortex.system.yml")
	}

	seen := map[string]string{}
	for i := range out {
		abs, err := filepath.Abs(out[i].Index)
		if err != nil {
			return nil, fmt.Errorf("system: resolve index path %q: %w", out[i].Index, err)
		}
		out[i].Index = abs
		if prev, dup := seen[out[i].Name]; dup {
			return nil, fmt.Errorf(
				"member %q is named twice (%s and %s); names identify repositories in every "+
					"line of the report and must be distinct", out[i].Name, prev, abs)
		}
		seen[out[i].Name] = abs
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---------------------------------------------------------------- indexes

// openIndex is one opened member database.
type openIndex struct {
	reg     *registry.Registry
	runID   int64
	failure string
	closeFn func()
}

// memberCatalogue opens each distinct index at most once.
//
// Same reason internal/cli/diff.go's catalogue exists: two members may name the
// same database (two views of one repository), and opening it twice would mean
// two connection pools over one SQLite file for no gain.
type memberCatalogue struct {
	byPath map[string]*openIndex
	order  []*openIndex
}

func (c *memberCatalogue) member(ctx stdctx.Context, e multirepo.EstateMember) multirepo.Member {
	m := multirepo.Member{Name: e.Name, IndexPath: e.Index}

	idx, ok := c.byPath[e.Index]
	if !ok {
		idx = openMemberIndex(ctx, e.Index)
		c.byPath[e.Index] = idx
		if idx.closeFn != nil {
			c.order = append(c.order, idx)
		}
	}
	if idx.failure != "" {
		m.Unavailable = idx.failure
		return m
	}
	m.Source, m.RunID = idx.reg, idx.runID
	return m
}

func (c *memberCatalogue) closeAll() {
	for _, idx := range c.order {
		idx.closeFn()
	}
}

// openMemberIndex opens one member read-only, or records why it could not.
//
// Every failure here is DATA, not an error: a missing index is the single most
// likely reason a system graph is wrong, and returning early would hide the
// other members' findings behind the one that could not be read. Each message
// names the command that would fix it, because the reader is usually not the
// person who owns that repository.
//
// Read-only, like every query path: the other checkout's index may be held by a
// `repocortex watch` over there, and a federated read has no business writing to
// it.
func openMemberIndex(ctx stdctx.Context, path string) *openIndex {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return &openIndex{failure: fmt.Sprintf(
			"no index at %s. Build one in that checkout: 'repocortex index --project <that checkout>'.",
			path)}
	} else if err != nil {
		return &openIndex{failure: fmt.Sprintf("cannot stat %s: %v", path, err)}
	}

	db, err := openReadOnly(ctx, path)
	if err != nil {
		// Covers the schema-version mismatch too, which is its own story: that
		// member was indexed by a build whose schema this one does not read, and
		// re-indexing it is the fix.
		return &openIndex{failure: fmt.Sprintf(
			"cannot read %s: %v. Re-index that checkout with this build of repocortex.", path, err)}
	}

	runs, err := listRuns(ctx, db)
	if err != nil {
		_ = db.Close()
		return &openIndex{failure: fmt.Sprintf("cannot list runs in %s: %v", path, err)}
	}
	var committed int64
	for _, r := range runs {
		if r.committed {
			committed = r.id
		}
	}
	if committed == 0 {
		_ = db.Close()
		// Distinguished from "no index" deliberately: the two look identical to
		// somebody staring at a member with no edges, and each has a different
		// fix. A schema with nothing committed in it means the index was
		// initialised and never populated.
		return &openIndex{failure: fmt.Sprintf(
			"%s holds %d run(s) but none is committed, so it exposes no facts. "+
				"Run 'repocortex index' in that checkout.", path, len(runs))}
	}

	return &openIndex{
		reg:     registry.New(db),
		runID:   committed,
		closeFn: func() { _ = db.Close() },
	}
}
