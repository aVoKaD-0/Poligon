package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/poligon/runner/contracts/controlv1"
	"github.com/poligon/runner/internal/execution"
	"github.com/poligon/runner/internal/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type wireFixture struct {
	pb.UnimplementedControlServiceServer
	url          string
	uploadID     string
	registration atomic.Int32
}

func (f *wireFixture) ClaimExecution(ctx context.Context, r *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	if metadata.ValueFromIncomingContext(ctx, "authorization")[0] != "Bearer control-test" || r.GetContext().GetAttemptNo() != 1 {
		return nil, status.Error(codes.PermissionDenied, "bad identity")
	}
	return &pb.LeaseResponse{Accepted: true, LeaseId: uuid.NewString()}, nil
}

func (f *wireFixture) PrepareArtifactUpload(_ context.Context, r *pb.ArtifactRequest) (*pb.ArtifactResponse, error) {
	if r.Producer != "runner" || r.GetContext().GetAttemptNo() != 1 {
		return nil, status.Error(codes.PermissionDenied, "bad identity")
	}
	sum, _ := hex.DecodeString(r.Sha256)
	return &pb.ArtifactResponse{UploadId: f.uploadID, ObjectKey: "runner/registered", SignedUrl: f.url + "?signature=test",
		ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339Nano), RequiredHeaders: map[string]string{
			"Content-Type": r.Mime, "Content-Length": strconv.FormatInt(r.SizeBytes, 10),
			"X-Amz-Checksum-Sha256": base64.StdEncoding.EncodeToString(sum),
		}}, nil
}

func (f *wireFixture) RegisterArtifact(_ context.Context, r *pb.ArtifactRequest) (*pb.ArtifactResponse, error) {
	if r.UploadId != f.uploadID || r.ObjectKey != "runner/registered" {
		return nil, status.Error(codes.InvalidArgument, "wrong grant")
	}
	if f.registration.Add(1) == 1 {
		return nil, status.Error(codes.Unavailable, "registration response lost")
	}
	return &pb.ArtifactResponse{ArtifactRef: &pb.ArtifactRef{
		ArtifactId: f.uploadID, Kind: r.Kind, Mime: r.Mime, Sha256: r.Sha256, SizeBytes: r.SizeBytes,
	}}, nil
}

func (f *wireFixture) ResolveArtifact(_ context.Context, r *pb.ArtifactRequest) (*pb.ArtifactResponse, error) {
	return &pb.ArtifactResponse{ArtifactRef: r.GetArtifactRef()}, nil
}

func TestGRPCAndSignedPUT(t *testing.T) {
	var uploads atomic.Int32
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		digest := sha256.Sum256(raw)
		if err != nil || r.Method != "PUT" || r.URL.Query().Get("signature") != "test" ||
			r.Header.Get("Content-Type") != "application/x-ndjson" || r.ContentLength != int64(len(raw)) ||
			r.Header.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(digest[:]) ||
			r.Header.Get("Authorization") != "" {
			http.Error(w, "bad signed upload", http.StatusBadRequest)
			return
		}
		uploads.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer storage.Close()
	f := &wireFixture{url: storage.URL, uploadID: uuid.NewString()}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	pb.RegisterControlServiceServer(server, f)
	go server.Serve(lis)
	defer server.Stop()
	conn, err := Dial(lis.Addr().String(), true, TLSFiles{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctl := &Control{RPC: pb.NewControlServiceClient(conn), Token: "control-test", Timeout: time.Second}
	scope := execution.Scope{ProjectID: uuid.NewString(), RunID: uuid.NewString(), SessionID: uuid.NewString(), Attempt: 1}
	claim, err := ctl.Claim(t.Context(), &pb.LeaseRequest{Context: scope.Proto(), WorkerId: uuid.NewString()})
	if err != nil || !claim.Accepted {
		t.Fatalf("claim failed: %v", err)
	}
	u, _ := url.Parse(storage.URL)
	a := &Artifacts{Control: ctl, Client: transport.HTTPClient(2, true, false), Hosts: map[string]bool{u.Host: true}, Local: true, PutTimeout: time.Second}
	defer a.Client.CloseIdleConnections()
	access := execution.Access{Scope: scope, Worker: uuid.NewString(), Lease: claim.LeaseId}
	for _, data := range [][]byte{[]byte("{}\n"), nil} {
		digest := sha256.Sum256(data)
		b := execution.Blob{Kind: "conversation", MIME: "application/x-ndjson", SHA: hex.EncodeToString(digest[:]), Size: int64(len(data)), Data: bytes.NewReader(data)}
		ref, err := a.Upload(t.Context(), access, b)
		if err != nil || !ref.Valid(b.Kind) {
			t.Fatalf("upload failed: %v", err)
		}
		if err := a.Resolve(t.Context(), access, ref); err != nil {
			t.Fatal(err)
		}
		b.SHA = hex.EncodeToString(make([]byte, 32))
		if _, err := a.Upload(t.Context(), access, b); err == nil {
			t.Fatal("bad checksum accepted")
		}
	}
	if uploads.Load() != 2 || f.registration.Load() != 3 {
		t.Fatalf("puts=%d, register calls=%d", uploads.Load(), f.registration.Load())
	}
}
