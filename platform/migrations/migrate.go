package migrations

import (
	"context"
	"embed"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var files embed.FS

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	tx, e := pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(70697000)"); e != nil {
		return e
	}
	var exists bool
	if e = tx.QueryRow(ctx, "SELECT to_regclass('public.schema_migrations') IS NOT NULL").Scan(&exists); e != nil {
		return e
	}
	if exists {
		var version int
		if e = tx.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); e != nil {
			return e
		}
		if version != 1 {
			return errors.New("unsupported database schema version")
		}
		return tx.Commit(ctx)
	}
	b, e := files.ReadFile("001_initial.sql")
	if e != nil {
		return e
	}
	if _, e = tx.Exec(ctx, string(b)); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
