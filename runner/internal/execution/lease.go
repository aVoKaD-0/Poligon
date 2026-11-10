package execution

import (
	"context"
	"sync/atomic"
	"time"

	pb "github.com/poligon/runner/contracts/controlv1"
)

type Control interface {
	Claim(context.Context, *pb.LeaseRequest) (*pb.LeaseResponse, error)
	Heartbeat(context.Context, *pb.LeaseRequest) (*pb.LeaseResponse, error)
	Plan(context.Context, *pb.PlanRequest) (*pb.LeaseResponse, error)
}

type leaseClockKey struct{}

// CheckLease fences a new side effect even if the heartbeat goroutine has not
// been scheduled since expiry. The monitor also cancels requests in flight.
func CheckLease(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if clock, ok := ctx.Value(leaseClockKey{}).(*atomic.Int64); ok && time.Now().UnixNano() >= clock.Load() {
		return errLease
	}
	return nil
}

func expiry(response *pb.LeaseResponse) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, response.GetLeaseExpiresAt())
	// Leave room for small clock skew and for the cancellation signal to propagate.
	return t.Add(-250 * time.Millisecond)
}

func monitorLease(ctx context.Context, ctl Control, req *pb.LeaseRequest, until time.Time,
	interval time.Duration, clock *atomic.Int64, lost, stopWork context.CancelCauseFunc, metrics *Metrics) {
	lose := func() {
		metrics.LeaseLost.Add(1)
		lost(errLease)
	}
	delay := interval
	for {
		remaining := time.Until(until)
		if remaining <= 0 {
			lose()
			return
		}
		timer := time.NewTimer(min(delay, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !time.Now().Before(until) {
			lose()
			return
		}
		call, cancel := context.WithDeadline(ctx, until)
		resp, err := ctl.Heartbeat(call, req)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if !time.Now().Before(until) {
			lose()
			return
		}
		if err != nil {
			delay = min(time.Second, interval)
			continue
		}
		next := expiry(resp)
		if !resp.GetAccepted() || resp.GetLeaseId() != req.LeaseId || !next.After(time.Now()) {
			lose()
			return
		}
		if resp.GetCancelRequested() {
			stopWork(errCancelled)
		}
		until = next
		clock.Store(until.UnixNano())
		delay = interval
	}
}
