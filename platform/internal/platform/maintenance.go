package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"io"
	"strconv"
)

func (a *App) SetProjectQuota(ctx context.Context, id, raw string) error {
	n, e := strconv.Atoi(raw)
	if !validID(id) || e != nil || n < 1 || n > 20 {
		return invalid("quota requires project UUID and concurrency 1-20")
	}
	return a.DB.Tx(ctx, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, "UPDATE projects SET concurrency=$2,revision=revision+1 WHERE id=$1", id, n)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return notFound()
		}
		return audit(ctx, tx, id, "operator", "project.quota", id)
	})
}
func (a *App) VerifyStorage(ctx context.Context) error {
	rows, e := queryJSON(ctx, a.DB.Pool, "SELECT to_jsonb(a) FROM artifact_index a WHERE NOT delete_pending ORDER BY id")
	if e != nil {
		return e
	}
	for _, v := range rows {
		input, e := a.Store.Client.GetObject(ctx, a.Store.Bucket, str(v, "object_key"), minio.GetObjectOptions{})
		if e != nil {
			return e
		}
		h := sha256.New()
		n, e := io.Copy(h, io.LimitReader(input, int64(integer(v, "size_bytes", 0))+1))
		input.Close()
		if e != nil {
			return e
		}
		if n != int64(integer(v, "size_bytes", 0)) || hex.EncodeToString(h.Sum(nil)) != str(v, "sha256") {
			return errors.New("registered artifact checksum mismatch")
		}
	}
	return nil
}

// Durable reservations prevent evaluation from starting during evidence removal.
func (a *App) PurgeExpired(ctx context.Context) error {
	e := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE artifact_index a SET delete_pending=true FROM runs r WHERE r.id=a.run_id AND r.status IN('completed','cancelled','failed') AND a.retain_until<now() AND NOT EXISTS(SELECT 1 FROM evaluation_jobs e JOIN sessions s ON s.id=e.session_id WHERE s.run_id=r.id AND e.status IN('queued','running'))`)
		if e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `UPDATE sessions s SET completeness='{"status":"expired"}' WHERE EXISTS(SELECT 1 FROM artifact_index a WHERE a.session_id=s.id AND a.attempt_no=s.current_attempt_no AND a.delete_pending)`)
		return e
	})
	if e != nil {
		return e
	}
	rows, e := queryJSON(ctx, a.DB.Pool, "SELECT to_jsonb(a) FROM artifact_index a WHERE delete_pending ORDER BY id LIMIT 1000")
	if e != nil {
		return e
	}
	for _, v := range rows {
		if e = a.Store.Client.RemoveObject(ctx, a.Store.Bucket, str(v, "object_key"), minio.RemoveObjectOptions{}); e != nil {
			return e
		}
		if _, e = a.DB.Pool.Exec(ctx, "DELETE FROM artifact_index WHERE id=$1 AND delete_pending", str(v, "id")); e != nil {
			return e
		}
	}
	return nil
}
