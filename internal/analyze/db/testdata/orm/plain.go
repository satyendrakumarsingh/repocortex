// Package plain is the negative fixture: real data access, no ORM. The raw SQL
// here is the other extractor's job, and ORMStatements must return nothing for
// it — reporting a table twice through two mechanisms would double every count
// `cortex db` prints.
package plain

import (
	"context"
	"database/sql"
)

type Repo struct {
	db *sql.DB
}

func (r *Repo) Find(ctx context.Context, id int64) error {
	row := r.db.QueryRowContext(ctx, "SELECT id, status FROM transactions WHERE id = ?", id)
	return row.Err()
}

func (r *Repo) Save(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, "UPDATE transactions SET status = ? WHERE id = ?", "PAID", id)
	return err
}

// Model, Table and Find are ordinary words. A scan that trusted them would put
// a table behind every one of these calls.
type Cache struct{ entries map[string]string }

func (c *Cache) Model() string          { return "cache" }
func (c *Cache) Table() string          { return "none" }
func (c *Cache) Delete(key string) bool { delete(c.entries, key); return true }
func (c *Cache) Save(key string)        { c.entries[key] = "" }
