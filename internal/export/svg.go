package export

import (
	"context"
	"fmt"
	"io"
	"math"

	"github.com/satyendrakumarsingh/repocortex/internal/analyze/metrics"
	"github.com/satyendrakumarsingh/repocortex/pkg/model"
)

// LayoutOptions controls the seeded force-directed layout.
type LayoutOptions struct {
	Width      int
	Height     int
	Iterations int
	// Seed pins the initial placement. Without it the same graph draws
	// differently on every export and two pictures cannot be compared.
	Seed int64
}

// DefaultLayoutOptions returns a seeded layout sized for a page.
func DefaultLayoutOptions() LayoutOptions {
	return LayoutOptions{Width: 1200, Height: 800, Iterations: 300, Seed: 42}
}

// nodeRadius is the drawn circle's radius. Lines are shortened by this much
// at both ends so an arrowhead marker lands next to the node it points at
// instead of being drawn underneath the circle.
const nodeRadius = 5.0

// svgExporter draws the graph with a seeded Fruchterman-Reingold layout: a
// static picture, not a serialization. Unlike DOT/GraphML/JSON/CSV it cannot
// hold every field of metrics.Node/metrics.Edge — see Lossy for the exact
// inventory of what a viewer of the picture can and cannot recover.
type svgExporter struct{}

func (svgExporter) Format() Format { return FormatSVG }

// Lossy names exactly what the drawing does and does not carry, field by
// field, because getting this wrong has been a defect in three earlier tasks
// in this package.
//
//   - Node.Key is drawn as a text label next to its circle.
//   - Edge.From/Edge.To are drawn as a line between the two node positions,
//     with an SVG marker-end arrowhead so direction survives looking at the
//     picture, not just reading source/target order in a file. Endpoints are
//     resolved with e.From/e.To explicitly (see package doc); a canonicalised
//     pair would silently reverse the arrow on any edge whose source sorts
//     after its target.
//   - Edge.Confidence gets a two-way visual split: exact and high draw a
//     solid line, medium and low draw a dashed one. That distinguishes "the
//     compiler/resolver was sure" from "this is a guess" at a glance, which is
//     the distinction a reader of a picture actually needs, but it collapses
//     the four-value scale to two — exact vs. high, and medium vs. low, are
//     not visually distinguishable from one another.
//   - Node.Kind, Edge.Kind and Edge.Weight are not drawn anywhere. A viewer
//     cannot tell a function from a package, a Calls edge from an Imports
//     edge, or a heavily-weighted edge from a rarely-taken one just by
//     looking at (or parsing) the output.
//
// An arrowhead marker was added specifically because a plain line carries no
// direction at all, and this package's one rule is that direction is never
// silently dropped or inferred from operand order (see export.go's package
// doc). Encoding Kind/Weight as SVG data-* attributes was considered and
// rejected: this exporter's contract is "the picture the canvas explorer
// would draw," and a reader who needs those fields back has DOT, GraphML,
// JSON or CSV available and should use one of those instead of parsing an
// SVG as if it were a data format.
func (svgExporter) Lossy() string {
	return "a picture, not a serialization: Key is drawn as a label, and From/To " +
		"as line endpoints with an arrowhead marker showing direction; Confidence is drawn " +
		"only as a two-way solid/dashed split (exact and high draw solid, medium and low draw " +
		"dashed), so the exact confidence level does not survive; Node.Kind, Edge.Kind and " +
		"Edge.Weight are not drawn anywhere and cannot be recovered from the output; and a " +
		"label is XML text, so runes XML 1.0 cannot encode are DELETED from it rather than " +
		"drawn (see stripXMLIllegal), which the GraphML exporter states for the same reason"
}

// Counts: every node gets a circle and every edge a line, so the drawing
// holds as many of each as g does. What it does not hold is the DETAIL of
// them, which Lossy states.
func (svgExporter) Counts(g metrics.Graph) (int, int) { return g.Order(), len(g.Edges()) }

// Write draws the graph with a seeded Fruchterman-Reingold layout.
//
// Initial placement is derived from each node's index on a golden-angle
// spiral rather than from a random source, so the layout is a pure function
// of the node list and the seed. Two exports of the same graph are
// byte-identical (see TestSVGIsByteStableAcrossRuns), which is what makes an
// SVG diffable and reviewable the way the other formats in this package are.
// That guarantee is scoped to a single platform: math.Sqrt is hardware-backed
// IEEE-754 and bit-identical across conforming platforms, and math.Hypot,
// math.Cos and math.Sin are pure-Go software implementations with no per-arch
// assembly, so cross-architecture stability is expected — but this has been
// reasoned about, not measured on a second machine.
//
// The repulsion pass below is O(iterations * n^2): every node repels every
// other node on every iteration. Measured with go test -bench on this
// machine, at the DefaultLayoutOptions Iterations of 300: n=100 -> 19ms,
// n=500 -> 305ms, n=1000 -> 1.17s, n=2000 -> 4.58s. The growth from n=1000 to
// n=2000 is a ~3.9x wall-time increase for a 2x node count, which is the n^2
// term making itself felt. Somewhere around 1,000-2,000 nodes this stops
// being "fine" and starts being a Write call a caller notices — well before
// a whole 100k-node index, but well within range for a large package's full
// dependency neighborhood. No cap is added here: capping node count would
// make this exporter silently draw a different, smaller graph than the one
// it was asked to draw. A caller exporting a graph that large should trim it
// (a subgraph, a k-hop neighborhood) before calling Write, the same way they
// would before asking any O(n^2) layout algorithm to lay it out.
func (svgExporter) Write(ctx context.Context, w io.Writer, g metrics.Graph) error {
	opts := DefaultLayoutOptions()
	keys := g.Keys()
	n := len(keys)
	if n == 0 {
		// Wrapped like every other exit in this function. A bare error from
		// here reaches the caller saying only "short write", with nothing to
		// say it came from an SVG export.
		if _, err := fmt.Fprintf(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d"></svg>`+"\n", opts.Width, opts.Height); err != nil {
			return fmt.Errorf("writing empty svg: %w", err)
		}
		return nil
	}

	idx := make(map[string]int, n)
	for i, k := range keys {
		idx[k] = i
	}

	xs, ys := initialSpiralPositions(keys, opts)

	edges := g.Edges()
	if err := layoutForceDirected(ctx, xs, ys, idx, edges, opts); err != nil {
		return err
	}
	// Final backstop: layoutForceDirected already clamps every node on every
	// iteration, so in the normal case this is a no-op. It is kept here
	// anyway because "the loop clamps it" was exactly the assumption that
	// broke once (the clamp lived inside the moved-node branch and was
	// skipped for a node that barely moved). A coordinate that slips past
	// every clamp is not rejected by anything downstream — SVG's viewBox
	// just clips it silently — so the emission point re-asserts the
	// invariant rather than trusting upstream code to have held it.
	for i := range xs {
		xs[i], ys[i] = clampToCanvas(xs[i], ys[i], opts)
	}

	bw := &errWriter{w: w}
	bw.printf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d">`+"\n",
		opts.Width, opts.Height, opts.Width, opts.Height)
	bw.printf("  <defs>\n")
	bw.printf(`    <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">` + "\n")
	bw.printf(`      <path d="M 0 0 L 10 5 L 0 10 z" fill="#8b8b8b"/>` + "\n")
	bw.printf("    </marker>\n")
	bw.printf("  </defs>\n")

	for _, e := range edges {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("writing svg edges: %w", err)
		}
		// From and To, explicitly — never a canonicalised pair. See the
		// package doc: a canonicalised pair silently reverses any edge whose
		// source sorts after its target, and here that would point the
		// arrowhead the wrong way.
		i, j := idx[e.From], idx[e.To]
		x1, y1, x2, y2 := shortenToRadius(xs[i], ys[i], xs[j], ys[j], nodeRadius)
		dash := ""
		if e.Confidence != model.ConfidenceExact && e.Confidence != model.ConfidenceHigh {
			dash = ` stroke-dasharray="4,3"`
		}
		bw.printf(`  <line x1="%.2f" y1="%.2f" x2="%.2f" y2="%.2f" stroke="#8b8b8b" stroke-width="1"%s marker-end="url(#arrow)"/>`+"\n",
			x1, y1, x2, y2, dash)
	}
	for i, key := range keys {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("writing svg nodes: %w", err)
		}
		bw.printf(`  <circle cx="%.2f" cy="%.2f" r="%.0f" fill="#4f46e5"/>`+"\n", xs[i], ys[i], nodeRadius)
		bw.printf(`  <text x="%.2f" y="%.2f" font-size="9" font-family="Helvetica">%s</text>`+"\n",
			xs[i]+7, ys[i]+3, xmlText(key))
	}
	bw.printf("</svg>\n")
	if bw.err != nil {
		return fmt.Errorf("writing svg: %w", bw.err)
	}
	return nil
}

// initialSpiralPositions places node i on a golden-angle spiral. This is a
// pure function of the node's index, opts.Width/Height and opts.Seed — never
// of map iteration order or math/rand — so the same graph always starts the
// force simulation from the same point and the whole layout is reproducible.
func initialSpiralPositions(keys []string, opts LayoutOptions) (xs, ys []float64) {
	n := len(keys)
	xs = make([]float64, n)
	ys = make([]float64, n)
	cx, cy := float64(opts.Width)/2, float64(opts.Height)/2
	radius := math.Min(cx, cy) * 0.8
	golden := math.Pi * (3 - math.Sqrt(5))
	for i := range keys {
		t := float64(i) + float64(opts.Seed%97)
		// sqrt(t+1)/sqrt(n+1) is meant to grow from ~0 to ~1 as t ranges over
		// a graph's nodes, so r grows from ~0 to ~radius. But t always starts
		// at the fixed offset opts.Seed%97 regardless of n, so for a small
		// graph (n well under that offset) the ratio overshoots 1 and r
		// overshoots radius — for n=1 it lands more than 4x past the
		// boundary. Capping at radius keeps the spiral shape for the graphs
		// it was already sized for and keeps every point within the canvas
		// for every other n, because radius = 0.8*min(cx,cy) is small enough
		// that cx±radius and cy±radius both stay inside [0,Width]x[0,Height].
		r := math.Min(radius, radius*math.Sqrt(t+1)/math.Sqrt(float64(n)+1))
		xs[i] = cx + r*math.Cos(golden*t)
		ys[i] = cy + r*math.Sin(golden*t)
		// Belt-and-suspenders: the r cap above already guarantees this is a
		// no-op, but every point this function hands back is clamped anyway
		// so "starts inside the canvas" is an invariant of the function's
		// output, not a derived fact that has to be re-proven by hand.
		xs[i], ys[i] = clampToCanvas(xs[i], ys[i], opts)
	}
	return xs, ys
}

// layoutForceDirected runs a seeded Fruchterman-Reingold simulation over xs/ys
// in place. It is O(opts.Iterations * n^2): every pair of nodes repels each
// other every iteration. See Write's doc comment for measured wall times.
func layoutForceDirected(ctx context.Context, xs, ys []float64, idx map[string]int, edges []metrics.Edge, opts LayoutOptions) error {
	n := len(xs)
	area := float64(opts.Width * opts.Height)
	k := math.Sqrt(area / float64(n))
	temp := float64(opts.Width) / 10

	dx := make([]float64, n)
	dy := make([]float64, n)
	for iter := 0; iter < opts.Iterations; iter++ {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("laying out svg: %w", err)
		}
		for i := range dx {
			dx[i], dy[i] = 0, 0
		}
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				ddx, ddy := xs[i]-xs[j], ys[i]-ys[j]
				d := math.Hypot(ddx, ddy)
				if d < 0.01 {
					d = 0.01
				}
				f := k * k / d
				ux, uy := ddx/d, ddy/d
				dx[i] += ux * f
				dy[i] += uy * f
				dx[j] -= ux * f
				dy[j] -= uy * f
			}
		}
		for _, e := range edges {
			// From and To, explicitly, matching the rule elsewhere in this
			// package: attraction must pull the actual source and target
			// together, not two nodes canonicalised into some other pair.
			i, j := idx[e.From], idx[e.To]
			ddx, ddy := xs[i]-xs[j], ys[i]-ys[j]
			d := math.Hypot(ddx, ddy)
			if d < 0.01 {
				d = 0.01
			}
			f := d * d / k
			ux, uy := ddx/d, ddy/d
			dx[i] -= ux * f
			dy[i] -= uy * f
			dx[j] += ux * f
			dy[j] += uy * f
		}
		for i := 0; i < n; i++ {
			// The clamp used to live inside this branch and only ran when a
			// node moved enough to clear the 0.01 threshold. A node that
			// never moves — e.g. the only node in the graph, with nothing to
			// repel and no attracting edge — never triggers it, so if its
			// starting position were ever off-canvas it would stay off-canvas
			// forever. Clamping unconditionally, on every node on every
			// iteration, makes "inside the canvas" an invariant of this loop
			// regardless of whether the node moved.
			d := math.Hypot(dx[i], dy[i])
			if d >= 0.01 {
				step := math.Min(d, temp)
				xs[i] += dx[i] / d * step
				ys[i] += dy[i] / d * step
			}
			xs[i], ys[i] = clampToCanvas(xs[i], ys[i], opts)
		}
		temp *= 0.95
	}
	return nil
}

// clampToCanvas pins (x, y) to stay within the viewBox, with a 10px margin
// so a node's circle (radius nodeRadius) never touches the edge either. It
// is applied at every point in this file that produces or updates a
// coordinate — initial placement, each simulation iteration, and once more
// as a final backstop in Write — because a coordinate that escapes any one
// of those points is not rejected by anything: SVG's viewBox just clips it,
// silently, and the output stays valid XML with an invisible node.
func clampToCanvas(x, y float64, opts LayoutOptions) (float64, float64) {
	return math.Max(10, math.Min(float64(opts.Width)-10, x)),
		math.Max(10, math.Min(float64(opts.Height)-10, y))
}

// shortenToRadius pulls both ends of the line from (x1,y1)-(x2,y2) in by r,
// so the drawn line stops at each node's circle boundary instead of running
// under it. Without this the arrowhead marker at the target end would be
// drawn underneath the target's circle and never be visible.
func shortenToRadius(x1, y1, x2, y2, r float64) (nx1, ny1, nx2, ny2 float64) {
	ddx, ddy := x2-x1, y2-y1
	d := math.Hypot(ddx, ddy)
	if d < 0.01 {
		return x1, y1, x2, y2
	}
	ux, uy := ddx/d, ddy/d
	return x1 + ux*r, y1 + uy*r, x2 - ux*r, y2 - uy*r
}
