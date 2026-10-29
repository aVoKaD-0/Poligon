package platform

import (
	"context"
	"net"
	"testing"

	pb "github.com/poligon/platform/gen/controlv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestGRPCServiceIdentityAndLeaseScope(t *testing.T) {
	f := newFixture(t)
	run := f.start(t)
	f.a.Config.ServiceTokens = map[string]string{"runner": "test-runner-service-token-32-bytes", "environment": "test-environment-service-token-32-bytes"}
	server, e := f.a.GRPCServer()
	if e != nil {
		t.Fatal(e)
	}
	listener := bufconn.Listen(1 << 20)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, e := grpc.NewClient("passthrough:///buf", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	client := pb.NewControlServiceClient(conn)
	ctx := context.Background()
	var sid, dispatch string
	if e = f.a.DB.Pool.QueryRow(ctx, "SELECT s.id::text,a.dispatch_id::text FROM sessions s JOIN execution_attempts a ON a.session_id=s.id WHERE s.run_id=$1", str(run, "run_id")).Scan(&sid, &dispatch); e != nil {
		t.Fatal(e)
	}
	request := &pb.LeaseRequest{Context: &pb.ExecutionContext{ProjectId: f.p, RunId: str(run, "run_id"), SessionId: sid, AttemptNo: 1}, WorkerId: ID(), DispatchId: dispatch}
	if _, e = client.ClaimExecution(ctx, request); status.Code(e) != codes.Unauthenticated {
		t.Fatal("anonymous claim accepted", e)
	}
	foreign := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+f.a.Config.ServiceTokens["environment"])
	if _, e = client.ClaimExecution(foreign, request); status.Code(e) != codes.PermissionDenied {
		t.Fatal("environment claimed execution", e)
	}
	runner := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+f.a.Config.ServiceTokens["runner"])
	response, e := client.ClaimExecution(runner, request)
	if e != nil || !response.Accepted {
		t.Fatal(response, e)
	}
	request.LeaseId = response.LeaseId
	heartbeat, e := client.HeartbeatExecution(runner, request)
	if e != nil || !heartbeat.Accepted {
		t.Fatal(heartbeat, e)
	}
	var plan []byte
	if e = f.a.DB.Pool.QueryRow(ctx, "SELECT plan FROM execution_attempts WHERE session_id=$1", sid).Scan(&plan); e != nil {
		t.Fatal(e)
	}
	_, h, e := canonical(obj(plan))
	if e != nil || h != response.RuntimePlanSha256 {
		t.Fatal("plan hash mismatch")
	}
	_, wireHash, e := canonical(obj(response.RuntimePlanJson))
	if e != nil || wireHash != h {
		t.Fatal("wire JSON changed the plan")
	}
	request.Context.ProjectId = ID()
	if _, e = client.HeartbeatExecution(runner, request); status.Code(e) != codes.NotFound {
		t.Fatal("cross-project lease accepted", e)
	}
}
