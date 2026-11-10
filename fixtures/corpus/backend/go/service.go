package service

import "os"

// A blank identifier is a definition to a tags query, and no reference can ever
// bind to it. 46 rows named `_` came from a real service.
var _ = os.Getenv

// envOrDefault reads a string env var (mirrors os.Getenv("...", "10000")-style
// defaulting used elsewhere; this comment once contributed a configuration key
// literally named "..." to the report).
func envOrDefault(key, def string) string {
	if v := os.Getenv("GO_REAL_KEY"); v != "" {
		return v
	}
	_, _ = key, def
	return def
}

// Query names a real table, so the SQL recogniser has something true to find
// alongside the CSS it must reject.
func Query() string {
	return "SELECT id, label FROM branches WHERE id = ?"
}
