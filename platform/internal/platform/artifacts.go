package platform

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/jackc/pgx/v5"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	pb "github.com/poligon/platform/gen/controlv1"
)

type ArtifactStore struct {
	Client, Signer *minio.Client
	Bucket         string
}

func NewArtifactStore(c Config) (*ArtifactStore, error) {
	opts := &minio.Options{Creds: credentials.NewStaticV4(c.S3AccessKey, c.S3SecretKey, ""), Secure: c.S3TLS}
	client, e := minio.New(c.S3Endpoint, opts)
	if e != nil {
		return nil, e
	}
	signer, e := minio.New(c.S3PublicEndpoint, opts)
	if e != nil {
		return nil, e
	}
	return &ArtifactStore{client, signer, c.S3Bucket}, nil
}
func (s *ArtifactStore) read(ctx context.Context, key string, size int64, sha string) ([]byte, error) {
	if size > 16<<20 {
		return nil, invalid("JSON artifact exceeds 16 MiB parsing limit")
	}
	o, e := s.Client.GetObject(ctx, s.Bucket, key, minio.GetObjectOptions{})
	if e != nil {
		return nil, e
	}
	defer o.Close()
	b, e := io.ReadAll(io.LimitReader(o, size+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) != size || hash(b) != sha {
		return nil, invalid("Artifact checksum or size mismatch")
	}
	return b, nil
}
func artifactOwner(kind string) string {
	switch kind {
	case "conversation", "tool_calls", "execution_manifest", "audio_track", "audio_alignment":
		return "runner"
	case "environment_snapshot", "environment_audit":
		return "environment"
	case "assessment":
		return "intelligence"
	case "diagnostics":
		return "any"
	}
	return ""
}
func artifactRef(v Object) Object {
	return Object{"artifact_id": v["id"], "sha256": v["sha256"], "size_bytes": stringNumber(v["size_bytes"]), "mime": v["mime"], "kind": v["kind"]}
}
func artifactProto(v Object) *pb.ArtifactRef {
	var n int64
	_ = json.Unmarshal(encode(v["size_bytes"]), &n)
	return &pb.ArtifactRef{ArtifactId: str(v, "id"), Sha256: str(v, "sha256"), SizeBytes: n, Mime: str(v, "mime"), Kind: str(v, "kind")}
}
func (c *Control) artifactAccess(ctx context.Context, r *pb.ArtifactRequest, write bool) error {
	if _, e := checkContext(ctx, c.App.DB.Pool, r.Context); e != nil {
		return e
	}
	s := service(ctx)
	if s == "environment" {
		if !write {
			return fail(403, "FORBIDDEN", "Environment cannot resolve arbitrary artifacts")
		}
		var ok bool
		e := c.App.DB.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM execution_attempts a JOIN sessions s ON s.id=a.session_id WHERE a.session_id=$1 AND a.attempt_no=$2 AND a.attempt_no=s.current_attempt_no AND a.status IN('leased','preparing','running') AND a.lease_expiry>now())", r.Context.SessionId, r.Context.AttemptNo).Scan(&ok)
		if e != nil {
			return e
		}
		if !ok {
			return fail(403, "LEASE_REQUIRED", "Execution inactive")
		}
		return nil
	}
	return c.authorizeLease(ctx, r.Context, r.WorkerId, r.LeaseId, r.EvaluationId)
}
func (c *Control) PrepareArtifactUpload(ctx context.Context, r *pb.ArtifactRequest) (*pb.ArtifactResponse, error) {
	if e := c.artifactAccess(ctx, r, true); e != nil {
		return nil, e
	}
	s := service(ctx)
	owner := artifactOwner(r.Kind)
	sha, e := hex.DecodeString(r.Sha256)
	if owner == "" || owner != "any" && owner != s || r.Producer != s || len(sha) != 32 || e != nil || hex.EncodeToString(sha) != r.Sha256 || r.SizeBytes < 0 || r.SizeBytes > 512<<20 || len(r.Mime) < 1 || len(r.Mime) > 200 {
		return nil, invalid("Invalid artifact metadata or producer")
	}
	if s == "intelligence" && !validID(r.EvaluationId) {
		return nil, invalid("evaluation_id required")
	}
	id := ID()
	ec := r.Context
	prefix := "projects/" + ec.ProjectId + "/runs/" + ec.RunId + "/sessions/" + ec.SessionId + "/attempts/" + strconv.Itoa(int(ec.AttemptNo)) + "/" + s + "/"
	if s == "intelligence" {
		prefix += r.EvaluationId + "/"
	}
	key := prefix + id
	var eval any
	if s == "intelligence" {
		eval = r.EvaluationId
	}
	_, e = c.App.DB.Pool.Exec(ctx, "INSERT INTO artifact_uploads(id,project_id,run_id,session_id,attempt_no,evaluation_id,producer,kind,object_key,size_bytes,sha256,mime,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,now()+interval '10 minutes')", id, ec.ProjectId, ec.RunId, ec.SessionId, ec.AttemptNo, eval, s, r.Kind, key, r.SizeBytes, r.Sha256, r.Mime)
	if e != nil {
		return nil, e
	}
	u, e := c.App.Store.Signer.PresignHeader(ctx, http.MethodPut, c.App.Store.Bucket, "uploads/"+id, 10*time.Minute, url.Values{}, http.Header{"Content-Type": []string{r.Mime}, "Content-Length": []string{strconv.FormatInt(r.SizeBytes, 10)}, "X-Amz-Checksum-Sha256": []string{base64.StdEncoding.EncodeToString(sha)}})
	if e != nil {
		return nil, e
	}
	return &pb.ArtifactResponse{UploadId: id, ObjectKey: key, SignedUrl: u.String(), ExpiresAt: utc().Add(10 * time.Minute).Format(time.RFC3339Nano), RequiredHeaders: map[string]string{"Content-Type": r.Mime, "Content-Length": strconv.FormatInt(r.SizeBytes, 10), "X-Amz-Checksum-Sha256": base64.StdEncoding.EncodeToString(sha)}}, nil
}
func (c *Control) RegisterArtifact(ctx context.Context, r *pb.ArtifactRequest) (*pb.ArtifactResponse, error) {
	if !validID(r.UploadId) {
		return nil, invalid("upload_id must be UUID")
	}
	if _, e := checkContext(ctx, c.App.DB.Pool, r.Context); e != nil {
		return nil, e
	}
	u, e := oneJSON(ctx, c.App.DB.Pool, "SELECT to_jsonb(u) FROM artifact_uploads u WHERE id=$1", r.UploadId)
	if e != nil {
		return nil, e
	}
	ec := r.Context
	if str(u, "project_id") != ec.ProjectId || str(u, "run_id") != ec.RunId || str(u, "session_id") != ec.SessionId || integer(u, "attempt_no", 0) != int(ec.AttemptNo) || str(u, "producer") != service(ctx) || r.Producer != service(ctx) {
		return nil, notFound()
	}
	if str(u, "kind") != r.Kind || str(u, "object_key") != r.ObjectKey || str(u, "sha256") != r.Sha256 || str(u, "mime") != r.Mime || stringNumber(u["size_bytes"]) != stringNumber(r.SizeBytes) || (service(ctx) == "intelligence" && str(u, "evaluation_id") != r.EvaluationId) {
		return nil, conflict("Upload metadata mismatch")
	}
	if id := str(u, "artifact_id"); id != "" {
		v, e := oneJSON(ctx, c.App.DB.Pool, "SELECT to_jsonb(a) FROM artifact_index a WHERE id=$1", id)
		return &pb.ArtifactResponse{ArtifactRef: artifactProto(v)}, e
	}
	if e := c.artifactAccess(ctx, r, true); e != nil {
		return nil, e
	}
	expiry, _ := time.Parse(time.RFC3339Nano, str(u, "expires_at"))
	if !expiry.After(utc()) {
		return nil, conflict("Upload grant expired")
	}
	// Signed URLs only address staging keys. Final objects are written from verified bytes,
	// so reusing an old PUT URL cannot mutate registered evidence.
	input, e := c.App.Store.Client.GetObject(ctx, c.App.Store.Bucket, "uploads/"+r.UploadId, minio.GetObjectOptions{})
	if e != nil {
		return nil, e
	}
	defer input.Close()
	tmp, e := os.CreateTemp("", "poligon-verified-*")
	if e != nil {
		return nil, e
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(input, r.SizeBytes+1))
	if e != nil {
		return nil, e
	}
	if n != r.SizeBytes || hex.EncodeToString(h.Sum(nil)) != r.Sha256 {
		return nil, invalid("Artifact checksum or size mismatch")
	}
	if _, e = tmp.Seek(0, 0); e != nil {
		return nil, e
	}
	if _, e = c.App.Store.Client.PutObject(ctx, c.App.Store.Bucket, r.ObjectKey, tmp, n, minio.PutObjectOptions{ContentType: r.Mime}); e != nil {
		return nil, e
	}
	var ref Object
	e = c.App.DB.Tx(ctx, func(tx pgx.Tx) error {
		fresh, e := oneJSON(ctx, tx, "SELECT to_jsonb(u) FROM artifact_uploads u WHERE id=$1", r.UploadId)
		if e != nil {
			return e
		}
		id := str(fresh, "artifact_id")
		if id == "" {
			id = ID()
			_, e = tx.Exec(ctx, "INSERT INTO artifact_index(id,upload_id,project_id,run_id,session_id,attempt_no,evaluation_id,producer,kind,object_key,size_bytes,sha256,mime,retain_until) SELECT $2,id,project_id,run_id,session_id,attempt_no,evaluation_id,producer,kind,object_key,size_bytes,sha256,mime,now()+interval '30 days' FROM artifact_uploads WHERE id=$1", r.UploadId, id)
			if e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, "UPDATE artifact_uploads SET artifact_id=$2 WHERE id=$1", r.UploadId, id); e != nil {
				return e
			}
		}
		ref, e = oneJSON(ctx, tx, "SELECT to_jsonb(a) FROM artifact_index a WHERE id=$1", id)
		return e
	})
	if e != nil {
		return nil, e
	}
	_ = c.App.Store.Client.RemoveObject(ctx, c.App.Store.Bucket, "uploads/"+r.UploadId, minio.RemoveObjectOptions{})
	return &pb.ArtifactResponse{ArtifactRef: artifactProto(ref)}, nil
}
func (c *Control) ResolveArtifact(ctx context.Context, r *pb.ArtifactRequest) (*pb.ArtifactResponse, error) {
	if e := c.artifactAccess(ctx, r, false); e != nil {
		return nil, e
	}
	if r.ArtifactRef == nil || !validID(r.ArtifactRef.ArtifactId) {
		return nil, invalid("artifact_ref required")
	}
	v, e := oneJSON(ctx, c.App.DB.Pool, "SELECT to_jsonb(a) FROM artifact_index a WHERE id=$1 AND project_id=$2 AND run_id=$3 AND session_id=$4 AND attempt_no=$5 AND NOT delete_pending", r.ArtifactRef.ArtifactId, r.Context.ProjectId, r.Context.RunId, r.Context.SessionId, r.Context.AttemptNo)
	if e != nil {
		return nil, e
	}
	if str(v, "sha256") != r.ArtifactRef.Sha256 || str(v, "kind") != r.ArtifactRef.Kind || stringNumber(v["size_bytes"]) != stringNumber(r.ArtifactRef.SizeBytes) || str(v, "mime") != r.ArtifactRef.Mime {
		return nil, invalid("Artifact ref mismatch")
	}
	if service(ctx) == "runner" && str(v, "producer") != "runner" && str(v, "producer") != "environment" {
		return nil, notFound()
	}
	if service(ctx) == "intelligence" && str(v, "producer") == "intelligence" && str(v, "evaluation_id") != r.EvaluationId {
		return nil, notFound()
	}
	u, e := c.App.Store.Signer.PresignedGetObject(ctx, c.App.Store.Bucket, str(v, "object_key"), 60*time.Second, url.Values{})
	if e != nil {
		return nil, e
	}
	return &pb.ArtifactResponse{SignedUrl: u.String(), ExpiresAt: utc().Add(time.Minute).Format(time.RFC3339Nano), ArtifactRef: artifactProto(v)}, nil
}
func (a *App) download(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	id, e := pk(r, "artifact_id")
	if e != nil {
		return nil, e
	}
	v, e := oneJSON(r.Context(), a.DB.Pool, "SELECT to_jsonb(a) FROM artifact_index a WHERE id=$1 AND project_id=$2 AND NOT delete_pending", id, r.PathValue("project_id"))
	if e != nil {
		return nil, e
	}
	u, e := a.Store.Signer.PresignedGetObject(r.Context(), a.Store.Bucket, str(v, "object_key"), time.Minute, url.Values{})
	if e != nil {
		return nil, e
	}
	http.Redirect(w, r, u.String(), 302)
	return nil, nil
}
func (a *App) readEvidence(ctx context.Context, env Envelope, ref Object, kind, checksum string) (Object, error) {
	id := str(ref, "artifact_id")
	if !validID(id) {
		return nil, invalid("Artifact ref missing")
	}
	v, e := oneJSON(ctx, a.DB.Pool, "SELECT to_jsonb(a) FROM artifact_index a WHERE id=$1 AND project_id=$2 AND run_id=$3 AND session_id=$4 AND attempt_no=$5 AND NOT delete_pending", id, env.ProjectID, env.RunID, env.SessionID, env.AttemptNo)
	if e != nil {
		return nil, e
	}
	if str(v, "kind") != kind || str(v, "sha256") != checksum || str(ref, "sha256") != checksum || str(v, "mime") != str(ref, "mime") || stringNumber(v["size_bytes"]) != str(ref, "size_bytes") {
		return nil, invalid("Artifact context/hash mismatch")
	}
	if kind == "assessment" && str(v, "evaluation_id") != str(env.Payload, "evaluation_id") {
		return nil, invalid("Assessment evaluation mismatch")
	}
	if a.Store == nil {
		return nil, fail(503, "STORAGE_UNAVAILABLE", "Artifact storage unavailable")
	}
	n := int64(integer(v, "size_bytes", 0))
	b, e := a.Store.read(ctx, str(v, "object_key"), n, checksum)
	if e != nil {
		return nil, e
	}
	if _, e = jsoncanonicalizer.Transform(b); e != nil {
		return nil, invalid("Artifact must contain valid JSON without duplicate keys")
	}
	o := obj(b)
	if o == nil {
		return nil, invalid("Artifact must be a JSON object")
	}
	if str(o, "project_id") != env.ProjectID || str(o, "run_id") != env.RunID || str(o, "session_id") != env.SessionID || integer(o, "attempt_no", 0) != env.AttemptNo {
		return nil, invalid("Artifact embedded context mismatch")
	}
	return o, nil
}
func (a *App) validateManifest(ctx context.Context, env Envelope, m Object) error {
	if e := validateArtifactSchema("execution-manifest", m); e != nil {
		return e
	}
	if integer(m, "schema_version", 0) != 1 || str(m, "execution_status") != "executed" {
		return invalid("Invalid manifest schema or execution status")
	}
	reasons := map[string]bool{"completed": true, "client_finished": true, "max_turns": true, "max_tool_calls": true, "session_deadline": true, "budget_exceeded": true, "agent_protocol_error": true}
	if !reasons[str(m, "termination_reason")] {
		return invalid("Invalid termination_reason")
	}
	for field, kind := range map[string]string{"conversation_ref": "conversation", "tool_calls_ref": "tool_calls", "environment_snapshot_ref": "environment_snapshot", "environment_audit_ref": "environment_audit"} {
		ref := sub(m, field)
		id := str(ref, "artifact_id")
		if !validID(id) {
			return invalid("Missing " + field)
		}
		v, e := oneJSON(ctx, a.DB.Pool, "SELECT to_jsonb(a) FROM artifact_index a WHERE id=$1 AND project_id=$2 AND run_id=$3 AND session_id=$4 AND attempt_no=$5 AND NOT delete_pending", id, env.ProjectID, env.RunID, env.SessionID, env.AttemptNo)
		if e != nil {
			return e
		}
		if str(v, "kind") != kind || str(v, "sha256") != str(ref, "sha256") || stringNumber(v["size_bytes"]) != str(ref, "size_bytes") || str(v, "mime") != str(ref, "mime") {
			return invalid("Manifest reference mismatch: " + field)
		}
	}
	return nil
}
