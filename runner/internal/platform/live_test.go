package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/poligon/runner/contracts"
	pb "github.com/poligon/runner/contracts/controlv1"
	"github.com/poligon/runner/internal/execution"
	"github.com/poligon/runner/internal/transport"
)

func TestLivePlatformControlAndArtifact(t *testing.T) {
	path := os.Getenv("POLIGON_PROBE_JOB")
	if path == "" {
		t.Skip("launch via the isolated upstream Platform fixture")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	job, err := execution.DecodeJob(execution.CommandSubject, raw)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := Dial(os.Getenv("POLIGON_PROBE_CONTROL"), true, TLSFiles{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	control := &Control{RPC: pb.NewControlServiceClient(conn), Token: os.Getenv("POLIGON_PROBE_TOKEN"), Timeout: 5 * time.Second}
	request := &pb.LeaseRequest{Context: job.Scope.Proto(), WorkerId: uuid.NewString(), DispatchId: job.Payload.Dispatch}
	claim, err := control.Claim(t.Context(), request)
	if err != nil || !claim.GetAccepted() {
		t.Fatalf("real claim failed: %v, reason=%s", err, claim.GetReason())
	}
	request.LeaseId = claim.LeaseId
	heartbeat, err := control.Heartbeat(t.Context(), request)
	if err != nil || !heartbeat.GetAccepted() || heartbeat.LeaseId != claim.LeaseId {
		t.Fatalf("real heartbeat failed: %v", err)
	}
	frozen, err := control.Plan(t.Context(), &pb.PlanRequest{Context: request.Context, WorkerId: request.WorkerId,
		LeaseId: claim.LeaseId, RuntimePlanRef: job.Payload.PlanRef, RuntimePlanSha256: job.Payload.PlanHash})
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := contracts.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execution.ParsePlan(job, frozen, schemas, 1<<20); err != nil {
		t.Fatal(err)
	}
	client := transport.HTTPClient(2, true, false)
	defer client.CloseIdleConnections()
	artifacts := Artifacts{Control: control, Client: client, Hosts: map[string]bool{strings.ToLower(os.Getenv("POLIGON_PROBE_STORAGE")): true},
		Local: true, PutTimeout: 5 * time.Second}
	access := execution.Access{Scope: job.Scope, Worker: request.WorkerId, Lease: claim.LeaseId}
	for _, data := range [][]byte{[]byte("{}\n"), nil} {
		digest := sha256.Sum256(data)
		ref, err := artifacts.Upload(t.Context(), access, execution.Blob{Kind: "conversation", MIME: "application/x-ndjson",
			SHA: hex.EncodeToString(digest[:]), Size: int64(len(data)), Data: bytes.NewReader(data)})
		if err != nil || !ref.Valid("conversation") {
			t.Fatalf("real registration failed: %v", err)
		}
		if err = artifacts.Resolve(t.Context(), access, ref); err != nil {
			t.Fatalf("real resolve failed: %v", err)
		}
	}
	competitor := &pb.LeaseRequest{Context: request.Context, WorkerId: uuid.NewString(), DispatchId: job.Payload.Dispatch}
	other, err := control.Claim(t.Context(), competitor)
	if err != nil || other.GetAccepted() || other.GetReason() != "ALREADY_CLAIMED" {
		t.Fatalf("duplicate claim did not fence worker: %v", err)
	}
}
