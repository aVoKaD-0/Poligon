package platform

import (
	"context"
	pb "github.com/poligon/platform/gen/controlv1"
	"sort"
	"sync"
	"testing"
	"time"
)

func Test20ConcurrentExecutionClaimsAndReadLatency(t *testing.T) {
	f := newFixture(t)
	ctx := context.WithValue(context.Background(), ctxKey("service"), "runner")
	if e := f.a.SetProjectQuota(ctx, f.p, "20"); e != nil {
		t.Fatal(e)
	}
	runs := []string{}
	for i := 0; i < 2; i++ {
		o := f.runRequest()
		o["repeats"], o["concurrency"] = 10, 20
		v := requireStatus(t, f.request("POST", "/api/v1/projects/"+f.p+"/runs", o, map[string]string{"Idempotency-Key": ID()}), 202)
		runs = append(runs, str(v, "run_id"))
	}
	jobs, e := queryJSON(ctx, f.a.DB.Pool, "SELECT jsonb_build_object('run_id',s.run_id,'session_id',s.id,'dispatch_id',a.dispatch_id) FROM sessions s JOIN execution_attempts a ON a.session_id=s.id WHERE s.project_id=$1", f.p)
	if e != nil {
		t.Fatal(e)
	}
	control := &Control{App: f.a}
	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(j Object) {
			defer wg.Done()
			req := &pb.LeaseRequest{Context: &pb.ExecutionContext{ProjectId: f.p, RunId: str(j, "run_id"), SessionId: str(j, "session_id"), AttemptNo: 1}, WorkerId: ID(), DispatchId: str(j, "dispatch_id")}
			r, e := control.ClaimExecution(ctx, req)
			if e != nil || !r.Accepted {
				t.Errorf("claim %v %v", r, e)
				return
			}
			req.LeaseId = r.LeaseId
			if r, e = control.HeartbeatExecution(ctx, req); e != nil || !r.Accepted {
				t.Errorf("heartbeat %v %v", r, e)
			}
		}(job)
	}
	wg.Wait()
	var active int
	if e = f.a.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM execution_attempts WHERE status='leased'").Scan(&active); e != nil || active != 20 {
		t.Fatalf("expected 20 concurrent active leases, got %d: %v", active, e)
	}
	latencies := make([]time.Duration, 100)
	for i := range latencies {
		start := time.Now()
		requireStatus(t, f.request("GET", "/api/v1/projects/"+f.p+"/runs/"+runs[i%2], nil, nil), 200)
		latencies[i] = time.Since(start)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	t.Logf("Platform only: 20 active execution leases, 100 REST reads, p50=%s p95=%s p99=%s; no Runner conversations or ML calls", latencies[49], latencies[94], latencies[98])
}
