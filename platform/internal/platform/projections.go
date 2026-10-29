package platform

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
)

// The immutable journal repairs missing live progress. Each source is verified on
// disk before inserting any records. Importing a diagnostic old attempt is safe:
// projection keys contain attempt_no and this operation never changes statuses.
func (a *App) rebuildProjection(ctx context.Context, event Envelope, manifest Object) error {
	for _, field := range []string{"conversation_ref", "tool_calls_ref"} {
		ref := sub(manifest, field)
		v, e := oneJSON(ctx, a.DB.Pool, "SELECT to_jsonb(a) FROM artifact_index a WHERE id=$1", str(ref, "artifact_id"))
		if e != nil {
			return e
		}
		if e = func() error {
			input, e := a.Store.Client.GetObject(ctx, a.Store.Bucket, str(v, "object_key"), minio.GetObjectOptions{})
			if e != nil {
				return e
			}
			defer input.Close()
			tmp, e := os.CreateTemp("", "poligon-projection-*")
			if e != nil {
				return e
			}
			defer os.Remove(tmp.Name())
			defer tmp.Close()
			h := sha256.New()
			expected := int64(integer(v, "size_bytes", 0))
			n, e := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(input, expected+1))
			if e != nil {
				return e
			}
			if n != expected || hex.EncodeToString(h.Sum(nil)) != str(v, "sha256") {
				return invalid("Journal checksum mismatch")
			}
			if _, e = tmp.Seek(0, 0); e != nil {
				return e
			}
			tx, e := a.DB.Pool.Begin(ctx)
			if e != nil {
				return e
			}
			defer tx.Rollback(ctx)
			scan := bufio.NewScanner(tmp)
			scan.Buffer(make([]byte, 4096), 64<<10)
			batch := &pgx.Batch{}
			for scan.Scan() {
				if _, e = jsoncanonicalizer.Transform(scan.Bytes()); e != nil {
					return invalid("Invalid NDJSON record")
				}
				record := obj(scan.Bytes())
				if record == nil || str(record, "project_id") != event.ProjectID || str(record, "run_id") != event.RunID || str(record, "session_id") != event.SessionID || integer(record, "attempt_no", 0) != event.AttemptNo || integer(record, "session_seq", 0) < 1 || !validID(str(record, "producer_event_id")) {
					return invalid("Invalid journal record")
				}
				batch.Queue("INSERT INTO progress_events(project_id,run_id,session_id,attempt_no,session_seq,producer_event_id,payload) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(session_id,attempt_no,session_seq) DO UPDATE SET payload=excluded.payload WHERE progress_events.producer_event_id=excluded.producer_event_id", event.ProjectID, event.RunID, event.SessionID, event.AttemptNo, integer(record, "session_seq", 0), str(record, "producer_event_id"), encode(record))
				if batch.Len() == 100 {
					if e = tx.SendBatch(ctx, batch).Close(); e != nil {
						return e
					}
					batch = &pgx.Batch{}
				}
			}
			if e = scan.Err(); e != nil {
				return invalid("Invalid NDJSON journal")
			}
			if batch.Len() > 0 {
				if e = tx.SendBatch(ctx, batch).Close(); e != nil {
					return e
				}
			}
			return tx.Commit(ctx)
		}(); e != nil {
			return e
		}
	}
	return nil
}
