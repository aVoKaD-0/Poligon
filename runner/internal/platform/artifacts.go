package platform

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	pb "github.com/poligon/runner/contracts/controlv1"
	"github.com/poligon/runner/internal/execution"
	"github.com/poligon/runner/internal/transport"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Artifacts struct {
	Control    *Control
	Client     *http.Client
	Hosts      map[string]bool
	Local      bool
	PutTimeout time.Duration
}

func artifactRequest(a execution.Access) *pb.ArtifactRequest {
	return &pb.ArtifactRequest{Context: a.Scope.Proto(), WorkerId: a.Worker, LeaseId: a.Lease, Producer: "runner"}
}

func refFromProto(p *pb.ArtifactRef) execution.Ref {
	return execution.Ref{ID: p.GetArtifactId(), SHA: p.GetSha256(), Size: p.GetSizeBytes(), MIME: p.GetMime(), Kind: p.GetKind()}
}

func (a *Artifacts) Upload(ctx context.Context, access execution.Access, b execution.Blob) (execution.Ref, error) {
	var none execution.Ref
	if err := execution.CheckLease(ctx); err != nil {
		return none, err
	}
	switch b.Kind {
	case "conversation", "tool_calls", "execution_manifest", "audio_track", "audio_alignment", "diagnostics":
	default:
		return none, errors.New("invalid Runner artifact kind")
	}
	if !execution.HashValid(b.SHA) || b.Size < 0 || b.Size > 512<<20 || b.Data == nil {
		return none, errors.New("invalid artifact data")
	}
	r := artifactRequest(access)
	r.Kind, r.Mime, r.Sha256, r.SizeBytes = b.Kind, b.MIME, b.SHA, b.Size
	rpc, cancel := a.Control.call(ctx)
	grant, err := a.Control.RPC.PrepareArtifactUpload(rpc, r)
	cancel()
	if err != nil {
		return none, err
	}
	expires, _ := time.Parse(time.RFC3339Nano, grant.GetExpiresAt())
	if !expires.After(time.Now()) || !execution.IDValid(grant.GetUploadId()) || grant.GetObjectKey() == "" {
		return none, errors.New("invalid upload grant")
	}
	if err = a.put(ctx, grant, b); err != nil {
		return none, err
	}
	r.UploadId, r.ObjectKey = grant.UploadId, grant.ObjectKey
	// Register is idempotent on the unchanged grant; a lost response can be retried.
	var registered *pb.ArtifactResponse
	for range 2 {
		if err = execution.CheckLease(ctx); err != nil {
			return none, err
		}
		rpc, cancel = a.Control.call(ctx)
		registered, err = a.Control.RPC.RegisterArtifact(rpc, r)
		cancel()
		if err == nil || (status.Code(err) != codes.Unavailable && status.Code(err) != codes.DeadlineExceeded) || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return none, err
	}
	ref := refFromProto(registered.GetArtifactRef())
	if !ref.Valid(b.Kind) || ref.SHA != b.SHA || ref.Size != b.Size || ref.MIME != b.MIME {
		return none, errors.New("registered artifact mismatch")
	}
	return ref, nil
}

func (a *Artifacts) put(ctx context.Context, grant *pb.ArtifactResponse, b execution.Blob) error {
	if err := execution.CheckLease(ctx); err != nil {
		return err
	}
	u, err := transport.Endpoint(grant.GetSignedUrl(), a.Local)
	if err != nil || !a.Hosts[strings.ToLower(u.Host)] {
		return errors.New("untrusted upload endpoint")
	}
	if _, err = b.Data.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var body io.Reader = http.NoBody
	if b.Size > 0 {
		body = io.LimitReader(b.Data, b.Size)
	}
	ctx, cancel := context.WithTimeout(ctx, a.PutTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), body)
	if err != nil {
		return errors.New("invalid signed PUT request")
	}
	req.ContentLength = b.Size
	var contentType, length, checksum bool
	for k, v := range grant.GetRequiredHeaders() {
		switch http.CanonicalHeaderKey(k) {
		case "Content-Length":
			if v != strconv.FormatInt(b.Size, 10) || length {
				return errors.New("upload length mismatch")
			}
			length = true
		case "Content-Type":
			if v != b.MIME || contentType {
				return errors.New("upload MIME mismatch")
			}
			contentType = true
			req.Header.Set(k, v)
		case "X-Amz-Checksum-Sha256":
			sum, _ := hex.DecodeString(b.SHA)
			if v != base64.StdEncoding.EncodeToString(sum) || checksum {
				return errors.New("upload checksum mismatch")
			}
			checksum = true
			req.Header.Set(k, v)
		default:
			return errors.New("unsupported signed PUT header")
		}
	}
	if !contentType || !length || !checksum {
		return errors.New("incomplete signed PUT headers")
	}
	res, err := a.Client.Do(req)
	if err != nil {
		return errors.New("signed PUT transport error")
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("signed PUT rejected")
	}
	return nil
}

func (a *Artifacts) Resolve(ctx context.Context, access execution.Access, ref execution.Ref) error {
	if err := execution.CheckLease(ctx); err != nil {
		return err
	}
	r := artifactRequest(access)
	r.ArtifactRef = &pb.ArtifactRef{ArtifactId: ref.ID, Sha256: ref.SHA, SizeBytes: ref.Size, Mime: ref.MIME, Kind: ref.Kind}
	rpc, cancel := a.Control.call(ctx)
	defer cancel()
	result, err := a.Control.RPC.ResolveArtifact(rpc, r)
	if err != nil {
		return err
	}
	if refFromProto(result.GetArtifactRef()) != ref {
		return errors.New("resolved artifact mismatch")
	}
	return nil
}
