package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "poligon/environment/gen/controlv1"
)

type artifact struct {
	data     []byte
	request  *pb.ArtifactRequest
	grant    *pb.ArtifactResponse
	uploaded bool
	ref      *pb.ArtifactRef
}

func digest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func newArtifact(s Scope, kind, mime string, b []byte) *artifact {
	return &artifact{data: b, request: &pb.ArtifactRequest{Context: s.proto(), Producer: "environment", Kind: kind, Mime: mime, SizeBytes: int64(len(b)), Sha256: digest(b)}}
}

func (a *artifact) grantExpired() bool {
	if a.grant == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, a.grant.ExpiresAt)
	return err == nil && !expires.After(time.Now())
}

func (a *artifact) clearGrant() {
	a.grant = nil
	a.uploaded = false
	a.request.UploadId = ""
	a.request.ObjectKey = ""
}

type uploader struct {
	control pb.ControlServiceClient
	token   string
	client  *http.Client
}

func newUploader(control pb.ControlServiceClient, token string) uploader {
	return uploader{control: control, token: token, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (u uploader) publish(ctx context.Context, a *artifact) error {
	if a.ref != nil {
		return nil
	}
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+u.token))
	if !a.uploaded && a.grantExpired() {
		a.clearGrant()
	}
	if a.grant == nil {
		g, err := u.control.PrepareArtifactUpload(ctx, a.request)
		if err != nil {
			return fmt.Errorf("prepare %s: %w", a.request.Kind, err)
		}
		if !uuid.MatchString(g.GetUploadId()) || g.GetSignedUrl() == "" || g.GetObjectKey() == "" {
			return fmt.Errorf("incomplete upload grant")
		}
		a.grant = g
		a.request.UploadId = g.UploadId
		a.request.ObjectKey = g.ObjectKey
	}
	if !a.uploaded {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, a.grant.SignedUrl, bytes.NewReader(a.data))
		if err != nil {
			return fmt.Errorf("invalid upload URL")
		}
		for k, v := range a.grant.RequiredHeaders {
			req.Header.Set(k, v)
		}
		res, err := u.client.Do(req)
		if err != nil {
			return fmt.Errorf("object storage upload unavailable")
		}
		res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return fmt.Errorf("object storage returned HTTP %d", res.StatusCode)
		}
		a.uploaded = true
	}
	registered, err := u.control.RegisterArtifact(ctx, a.request)
	if err != nil {
		// Control returns an existing ref before checking expiration. An explicit
		// conflict after expiry confirms this grant cannot finish registration.
		if a.grantExpired() && status.Code(err) == codes.FailedPrecondition && status.Convert(err).Message() == "VERSION_CONFLICT" {
			a.clearGrant()
		}
		return fmt.Errorf("register %s: %w", a.request.Kind, err)
	}
	ref := registered.GetArtifactRef()
	if !uuid.MatchString(ref.GetArtifactId()) || ref.GetSha256() != a.request.Sha256 || ref.GetSizeBytes() != a.request.SizeBytes || ref.GetMime() != a.request.Mime || ref.GetKind() != a.request.Kind {
		return status.Error(codes.DataLoss, "Control returned mismatched artifact ref")
	}
	a.ref = proto.CloneOf(ref)
	return nil
}
