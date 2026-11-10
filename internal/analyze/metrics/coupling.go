package metrics

import (
	"context"
	"fmt"
)

// Degree is the raw in/out edge count for one node.
type Degree struct {
	Key string
	In  int
	Out int
}

// Degrees returns per-node in/out degree, key-ascending.
func Degrees(ctx context.Context, g Graph) ([]Degree, error) {
	out := make([]Degree, 0, g.Order())
	for _, k := range g.keys {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("computing degrees: %w", err)
		}
		out = append(out, Degree{Key: k, In: len(g.in[k]), Out: len(g.out[k])})
	}
	return out, nil
}

// Coupling reports afferent/efferent coupling and Martin's instability.
//
// Instability I = Ce / (Ca + Ce): 0 means nothing depends outward and much
// depends inward (stable, expensive to change); 1 means the reverse. An
// isolated node has no ratio to report, and is defined as 0 rather than NaN so
// the value is sortable and serialisable.
type Coupling struct {
	Key         string
	FanIn       int
	FanOut      int
	Instability float64
}

// CouplingOf returns coupling metrics per node, key-ascending.
func CouplingOf(ctx context.Context, g Graph) ([]Coupling, error) {
	degs, err := Degrees(ctx, g)
	if err != nil {
		return nil, err
	}
	out := make([]Coupling, 0, len(degs))
	for _, d := range degs {
		c := Coupling{Key: d.Key, FanIn: d.In, FanOut: d.Out}
		if total := d.In + d.Out; total > 0 {
			c.Instability = float64(d.Out) / float64(total)
		}
		out = append(out, c)
	}
	return out, nil
}
