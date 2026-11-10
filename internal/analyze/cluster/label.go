package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
)

// Label names one cluster, and says how the name was derived.
//
// Method is not decoration. A name taken from a shared directory prefix is a
// fact about the tree; a name taken from the most central member is a guess
// that happens to be useful. A reader who cannot tell them apart will treat
// both as an architectural claim, which is exactly the failure mode that makes
// auto-generated architecture reports untrustworthy.
type Label struct {
	ClusterID string
	Name      string
	Method    string
	Members   int
}

// Labels names every cluster, cluster-id-ascending.
//
// Two strategies, in order of evidence strength:
//
//  1. "path-prefix" — every member's key has a real directory component (the
//     part of the path before the filename), and those directories agree.
//     This is a fact about the tree. A key with no "/" at all — a bare
//     symbol, a malformed key, two members that merely share a filename
//     stem — contributes no directory and disqualifies the cluster from this
//     label; see sharedDirPrefix.
//  2. "top-ranked-member" — no shared directory (including the case where no
//     member has one), so the cluster is named after its highest-ranked
//     member. This is a heuristic, and says so.
//
// ranks may be nil, in which case strategy 2 falls back to the smallest member
// key so the output stays deterministic.
func Labels(ctx context.Context, g metrics.Graph, assignments []Assignment, ranks []metrics.Score) ([]Label, error) {
	members := map[string][]string{}
	for _, a := range assignments {
		members[a.ClusterID] = append(members[a.ClusterID], a.Key)
	}
	ids := make([]string, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	rank := make(map[string]float64, len(ranks))
	for _, r := range ranks {
		rank[r.Key] = r.Value
	}

	out := make([]Label, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("labelling clusters: %w", err)
		}
		ms := members[id]
		sort.Strings(ms)

		if prefix := sharedDirPrefix(ms); prefix != "" {
			out = append(out, Label{ClusterID: id, Name: prefix, Method: "path-prefix", Members: len(ms)})
			continue
		}

		best := ms[0]
		bestRank := rank[best]
		for _, m := range ms {
			if r := rank[m]; r > bestRank || (r == bestRank && m < best) {
				best, bestRank = m, r
			}
		}
		out = append(out, Label{ClusterID: id, Name: best, Method: "top-ranked-member", Members: len(ms)})
	}
	return out, nil
}

// sharedDirPrefix returns the longest directory prefix common to every key,
// or "" when the members do not agree beyond the root — or when any member
// has no directory to agree with in the first place.
//
// A key is normally "path/to/file.go:Symbol". split strips the ":Symbol"
// suffix and then the filename, leaving the directory. But nothing upstream
// guarantees that shape: a malformed key, a bare symbol name, or any key
// without a "/" has no filename-vs-directory split to make. Treating the
// leftover text as a one-segment "directory" in that case would let a guess
// (or a parser bug) be reported as "path-prefix" — a stated fact about the
// tree — which is the one thing this label must never do. So split reports
// "no directory" (nil) rather than inventing one, and that value propagates
// to disqualify the whole cluster (common becomes empty, and the function
// returns "").
//
// This also disqualifies the case where two members share only a filename
// stem with no directory at all (e.g. "foo:A" and "foo:B", both keys with no
// "/"): that shared "foo" is a filename, not a directory, and labelling it
// "path-prefix" would still overclaim. Callers get "top-ranked-member"
// instead, which is honest about being a guess.
func sharedDirPrefix(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	split := func(k string) []string {
		path := k
		if i := strings.LastIndex(k, ":"); i >= 0 {
			path = k[:i]
		}
		i := strings.LastIndex(path, "/")
		if i < 0 {
			// No "/" means no directory component exists to report — only a
			// bare stem (or the raw key, if there was no colon either).
			// Returning that as a fake one-segment directory is the bug this
			// guards against.
			return nil
		}
		path = path[:i]
		if path == "" {
			return nil
		}
		return strings.Split(path, "/")
	}
	common := split(keys[0])
	for _, k := range keys[1:] {
		parts := split(k)
		n := len(common)
		if len(parts) < n {
			n = len(parts)
		}
		i := 0
		for i < n && common[i] == parts[i] {
			i++
		}
		common = common[:i]
		if len(common) == 0 {
			return ""
		}
	}
	return strings.Join(common, "/")
}
