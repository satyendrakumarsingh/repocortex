package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/satyendrakumarsingh/repocortex/internal/security"
)

// The six PILLAR screens: the ones that answer a question no other tool in this
// space answers at all.
//
// # Why they are registered from here and not from routes()
//
// They were built alongside a rewrite of the package's own file and could not
// share it. That is a scheduling accident, not a design: the composition root
// calls registerPillarRoutes from routes() and the six behave exactly like the
// screens registered there. Nothing else lives in this file except the two
// helpers three of these screens need to name a file inside the repository.
//
// # What the six have in common, and it is the reason they are grouped
//
// Every one of them is EMPTY on a repository that has not done a second thing:
// imported a compiler index, indexed twice, exported a trace, written a bench
// case file, or declared an estate. An empty screen that renders an empty list
// teaches the reader the answer is "none", when the answer is "not measured
// here" — the single misreading this whole product exists to prevent. So the
// guidance path is the MAIN path in each of these handlers, it names the exact
// command that populates the screen, and it always reports what the index DOES
// cover so that absence is never mistaken for evidence of absence.

// registerPillarRoutes attaches the pillar endpoints to mux.
func (s *Server) registerPillarRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/accuracy", s.handleAccuracy)
	mux.HandleFunc("GET /api/diff", s.handleFactDiff)
	mux.HandleFunc("GET /api/who", s.handleOwnership)
	mux.HandleFunc("GET /api/trace", s.handleTrace)
	mux.HandleFunc("GET /api/bench", s.handleBench)
	mux.HandleFunc("GET /api/system", s.handleEstate)
}

// ---------------------------------------------------------------- input files
//
// Three of these screens read an artefact the index does not hold: a trace
// export, a benchmark case file, an estate manifest. The portal has no way to
// be handed one — it is constructed with a Deps and a set of Options, and none
// of them carries a path — so the request names it.
//
// THE PATH IS CONFINED TO THE INDEXED REPOSITORY, deliberately and without an
// escape hatch. A local portal that will read any file a query string names is
// an arbitrary-file-read primitive one DNS-rebinding mistake away from being
// remote, and the loopback Host guard is one control rather than two. Confining
// it costs nothing real: the three artefacts are things a team commits beside
// the code, which is also why each has a conventional name this resolves to
// when the request does not say.

// FileRef is the artefact a screen tried to read, echoed on every response so a
// reader who sees an empty screen knows exactly which file was looked for and
// whether they chose it or the portal did.
type FileRef struct {
	// Requested is the value the caller sent, empty when it defaulted.
	Requested string `json:"requested,omitempty"`

	// Path is the repository-relative path actually resolved.
	Path string `json:"path"`

	// Absolute is the file the portal opened, or would have.
	Absolute string `json:"absolute,omitempty"`

	// root is the repository root Absolute was contained under; readRepoFile
	// re-reads through it with os.Root so the read itself cannot be redirected.
	root string

	// Defaulted is true when the caller named nothing and the conventional
	// name was used. Stated as a field rather than inferred from Requested
	// being empty, because "you asked for this and it is missing" and "we
	// guessed and the guess is missing" are different messages.
	Defaulted bool `json:"defaulted"`

	// DefaultedFrom is the sentence saying what was defaulted to and why.
	DefaultedFrom string `json:"defaulted_from,omitempty"`

	// Found is whether the file exists and is readable.
	Found bool `json:"found"`

	// Problem is why it could not be read, written for the reader who has to
	// fix it. Empty when Found.
	Problem string `json:"problem,omitempty"`
}

// repoFile resolves a caller-named artefact against the indexed repository.
//
// Returns a FileRef that is ALWAYS renderable: a rejected path, a missing file
// and a readable one all come back as the same shape with a different Problem,
// because a handler that had to distinguish three error paths would eventually
// forget one and answer with an empty body.
func (s *Server) repoFile(r *http.Request, param, conventional string) FileRef {
	ref := FileRef{Requested: strings.TrimSpace(r.URL.Query().Get(param))}

	rel := ref.Requested
	if rel == "" {
		rel = conventional
		ref.Defaulted = true
		ref.DefaultedFrom = fmt.Sprintf(
			"No ?%s= was given, so the portal looked for %s in the repository root — the "+
				"conventional name for this file.", param, conventional)
	}
	ref.Path = filepath.ToSlash(filepath.Clean(rel))

	root := strings.TrimSpace(s.deps.Config.RepoRoot)
	if root == "" {
		ref.Problem = "This portal was started without a repository root, so it cannot locate a " +
			"file relative to the indexed tree. Start it from the repository, or with --project."
		return ref
	}
	if filepath.IsAbs(rel) {
		ref.Problem = fmt.Sprintf(
			"%q is an absolute path. The portal reads only files inside the indexed repository "+
				"(%s); name the file relative to it.", rel, root)
		return ref
	}

	// ValidatePath rejects "..", absolute paths and symlinks (directory or
	// dangling) that lead outside the root; the lexical check this replaced
	// let a symlink inside the repository read anywhere.
	abs, err := security.ValidatePath(root, ref.Path)
	if err != nil {
		ref.Problem = fmt.Sprintf(
			"%q resolves outside the indexed repository (%s). The portal reads only files inside "+
				"it — a local page that will open any path a URL names is an arbitrary-file read.",
			rel, root)
		return ref
	}
	ref.root = root
	ref.Absolute = abs
	return ref
}
