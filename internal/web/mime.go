package web

import "mime"

// Go resolves a file's Content-Type from the operating system's tables, and on
// Windows that is the registry, which does not reliably know .mjs. A browser
// refuses an ES module served with the wrong type and renders an empty page
// without an error, so the types the portal's assets depend on are pinned here
// instead of left to whichever machine runs the server.
func init() {
	for ext, typ := range map[string]string{
		".mjs": "text/javascript; charset=utf-8",
		".js":  "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8",
		".svg": "image/svg+xml",
	} {
		// AddExtensionType only fails on a malformed extension or type.
		if err := mime.AddExtensionType(ext, typ); err != nil {
			panic("web: register " + ext + ": " + err.Error())
		}
	}
}
