package platform

import (
	"context"
	"testing"
)

func TestRetentionDoesNotDeleteActiveEvidence(t *testing.T) {
	f := newFixture(t)
	f.a.Store = fakeS3(t)
	ctx, c, lease := claimFixture(t, f)
	ref := uploadArtifact(t, c, ctx, lease, "diagnostics", []byte(`{"diagnostic":"test"}`))
	if e := f.a.VerifyStorage(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e := f.a.DB.Pool.Exec(ctx, "UPDATE artifact_index SET retain_until=now()-interval '1 day' WHERE id=$1", ref.ArtifactId); e != nil {
		t.Fatal(e)
	}
	if e := f.a.PurgeExpired(ctx); e != nil {
		t.Fatal(e)
	}
	var count int
	if e := f.a.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM artifact_index WHERE id=$1 AND NOT delete_pending", ref.ArtifactId).Scan(&count); e != nil || count != 1 {
		t.Fatal("active run evidence deleted", e)
	}
	requireStatus(t, f.request("POST", "/api/v1/projects/"+f.p+"/runs/"+lease.Context.RunId+"/cancel", nil, nil), 202)
	if _, e := f.a.DB.Pool.Exec(ctx, "UPDATE execution_attempts SET lease_expiry=now()-interval '1 second' WHERE session_id=$1", lease.Context.SessionId); e != nil {
		t.Fatal(e)
	}
	if e := f.a.Supervise(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e := f.a.PurgeExpired(ctx); e != nil {
		t.Fatal(e)
	}
	if e := f.a.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM artifact_index WHERE id=$1", ref.ArtifactId).Scan(&count); e != nil || count != 0 {
		t.Fatal("expired terminal evidence not removed", e)
	}
}
