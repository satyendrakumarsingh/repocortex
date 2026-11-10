package web

import "embed"

// assets is the whole portal: one HTML shell, one stylesheet, one script.
//
// # Why it is embedded rather than built
//
// No npm, no bundler, no CDN, no build step. The binary must work on a machine
// with no Node and no network — a developer inspecting a private repository is
// exactly the person who should not need to fetch a framework from a third party
// to look at their own code, and a portal that renders a codebase's structure
// must not also open an outbound connection while doing it. The Content-Security
// -Policy in withGuards states the same constraint at the protocol level, so a
// remote reference added later fails visibly instead of working in development
// and leaking in production.
//
// The consequence is that the graph renderer is hand-written Canvas 2D rather
// than a library. That is not only a licensing convenience: these graphs are
// LABEL-DOMINANT — a symbol node is mostly its name — and text is where Canvas 2D
// beats WebGL outright. A WebGL renderer must either rasterise every label into
// an atlas or overlay DOM, and at the node counts this portal bounds itself to
// (a few hundred containers, a low thousand symbols) the GPU wins nothing while
// costing shader complexity, a font atlas, and a second code path for the text
// that is the actual content.
//
//go:embed assets
var assets embed.FS
