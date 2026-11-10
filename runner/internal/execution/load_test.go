package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDialogueLimits(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		limits     Limits
		reply      func(context.Context, Input) (Response, error)
	}{
		{"max_turns", "max_turns", Limits{Turns: 1, Tools: 2}, func(context.Context, Input) (Response, error) {
			return Response{Messages: []string{"reply"}}, nil
		}},
		{"max_tool_calls", "max_tool_calls", Limits{Turns: 3, Tools: 1}, func(_ context.Context, i Input) (Response, error) {
			if i.Message == nil {
				return Response{Messages: []string{"got result"}}, nil
			}
			return Response{Calls: []Call{{ID: "first", Name: "list_orders", Arguments: json.RawMessage(`{}`)},
				{ID: "second", Name: "list_orders", Arguments: json.RawMessage(`{}`)}}}, nil
		}},
		{"budget_exceeded", "budget_exceeded", Limits{Turns: 3, Tools: 2, Budget: new(int64)}, func(context.Context, Input) (Response, error) {
			cost := int64(1)
			return Response{Messages: []string{"reply"}, Cost: &cost}, nil
		}},
		{"session_deadline", "session_deadline", Limits{Turns: 3, Tools: 2}, func(ctx context.Context, _ Input) (Response, error) {
			<-ctx.Done()
			return Response{}, ctx.Err()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, j, c, p, _ := newEngine(t)
			plan, err := ParsePlan(j, c.plan, e.Schemas, e.PlanLimit)
			if err != nil {
				t.Fatal(err)
			}
			plan.Limits = tc.limits
			conversation, err := openJournal(e.TempDir)
			if err != nil {
				t.Fatal(err)
			}
			defer conversation.close()
			tools, err := openJournal(e.TempDir)
			if err != nil {
				t.Fatal(err)
			}
			defer tools.close()
			d := &dialogue{engine: e, rec: &recorder{events: newEvents(t.Context(), j, p), conversation: conversation, tools: tools, limit: 1 << 20}, plan: plan, callIDs: make(map[string]struct{})}
			ctx := t.Context()
			if tc.want == "session_deadline" {
				deadline, cancel := context.WithTimeoutCause(ctx, 25*time.Millisecond, errSessionDeadline)
				defer cancel()
				ctx = deadline
			}
			reason, issue := d.loop(ctx, fakeAgent{tc.reply}, fakeSim{}, fakeEnvironment{})
			if reason != tc.want || issue.code != "" {
				t.Fatalf("%s %+v", reason, issue)
			}
			if tc.want == "max_tool_calls" && d.toolCalls != 1 {
				t.Fatal("tool limit failed")
			}
		})
	}
}

type discardBroker struct{}

func (discardBroker) Publish(_ context.Context, _, _ string, _ []byte) error { return nil }

func BenchmarkEventRecord(b *testing.B) {
	e, j, _, _, _ := newEngine(b)
	c, err := openJournal(e.TempDir)
	if err != nil {
		b.Fatal(err)
	}
	defer c.close()
	rec := &recorder{events: newEvents(b.Context(), j, discardBroker{}), conversation: c, tools: &journal{}, limit: 1 << 40}
	payload := map[string]string{"text": "simulated customer message"}
	b.ReportAllocs()
	for b.Loop() {
		if err = rec.append(b.Context(), c, "client.message", payload); err != nil {
			b.Fatal(err)
		}
		if c.size >= 4<<20 {
			if err = c.out.Flush(); err != nil {
				b.Fatal(err)
			}
			if _, err = c.f.Seek(0, io.SeekStart); err != nil {
				b.Fatal(err)
			}
			c.hash.Reset()
			c.size = 0
		}
	}
}

// TestLoadSessions is opt-in to keep the ordinary unit suite fast.
func TestLoadSessions(t *testing.T) {
	durationText := os.Getenv("POLIGON_RUNNER_LOAD")
	if durationText == "" {
		t.Skip("set POLIGON_RUNNER_LOAD, for example 3s")
	}
	duration, err := time.ParseDuration(durationText)
	if err != nil || duration < time.Second {
		t.Fatal("load duration must be at least one second")
	}
	for _, parallel := range []int{1, 10, 50, 100} {
		t.Run(strconv.Itoa(parallel), func(t *testing.T) { runLoad(t, parallel, duration, false) })
	}
	if value := os.Getenv("POLIGON_RUNNER_SOAK"); value != "" {
		soak, err := time.ParseDuration(value)
		if err != nil || soak < 10*time.Second {
			t.Fatal("soak duration must be at least 10 seconds")
		}
		t.Run("soak", func(t *testing.T) { runLoad(t, 20, soak, true) })
	}
}

func runLoad(t *testing.T, parallel int, duration time.Duration, degrade bool) {
	t.Helper()
	e, _, _, _, _ := newEngine(t)
	e.Agents = fakeAgent{func(ctx context.Context, _ Input) (Response, error) {
		timer := time.NewTimer(40 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return Response{}, ctx.Err()
		case <-timer.C:
			return Response{Messages: []string{"response"}, Finished: true}, nil
		}
	}}
	e.Publisher = discardBroker{}
	type input struct {
		j        Job
		response *testControl
	}
	jobs := make([]input, parallel)
	for i := range jobs {
		job, plan := planFor(t)
		jobs[i] = input{job, &testControl{plan: plan, ttl: 30 * time.Second}}
	}
	runtime.GC()
	var startMem, endMem, peakMem runtime.MemStats
	runtime.ReadMemStats(&startMem)
	initialG := runtime.NumGoroutine()
	var total, errorsFound, beats, cancels, failed atomic.Int64
	latencies := make([][]time.Duration, parallel)
	start := time.Now()
	deadline := start.Add(duration)
	cpu := cpuSeconds()
	var group sync.WaitGroup
	for i, item := range jobs {
		group.Go(func() {
			n := 0
			for time.Now().Before(deadline) {
				n++
				started := time.Now()
				ctl := item.response
				ctl.claimed.Store(false)
				ctl.beats.Store(0)
				ctl.cancel = degrade && n%23 == 0
				engine := *e
				engine.Control = ctl
				engine.Evidence = &testEvidence{data: make(map[string][]byte), fail: degrade && n%31 == 0}
				j := item.j
				j.OccurredAt = started
				if engine.Run(t.Context(), j) != Ack {
					errorsFound.Add(1)
				}
				if ctl.cancel {
					cancels.Add(1)
				}
				if degrade && n%31 == 0 && !ctl.cancel {
					failed.Add(1)
				}
				beats.Add(int64(ctl.beats.Load()))
				total.Add(1)
				if len(latencies[i]) < 1024 {
					latencies[i] = append(latencies[i], time.Since(started))
				}
			}
		})
	}
	complete := make(chan struct{})
	go func() { group.Wait(); close(complete) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	peakG := initialG
	sample := func() {
		peakG = max(peakG, runtime.NumGoroutine())
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		peakMem.HeapInuse = max(peakMem.HeapInuse, m.HeapInuse)
	}
wait:
	for {
		select {
		case <-ticker.C:
			sample()
		case <-complete:
			break wait
		}
	}
	finish := time.Now()
	cpu = cpuSeconds() - cpu
	runtime.GC()
	runtime.ReadMemStats(&endMem)
	endG := runtime.NumGoroutine()
	remain, err := os.ReadDir(e.TempDir)
	if err != nil || len(remain) != 0 || e.Metrics.Active.Load() != 0 || endG > initialG+2 {
		t.Fatalf("resources retained: files=%d goroutines %d -> %d, err=%v", len(remain), initialG, endG, err)
	}
	if errorsFound.Load() != 0 || beats.Load() == 0 || int64(e.Metrics.Cancelled.Load()) != cancels.Load() ||
		int64(e.Metrics.Failed.Load()) != failed.Load() {
		t.Fatalf("outcomes mismatch: retry=%d heartbeats=%d cancelled=%d failed=%d", errorsFound.Load(), beats.Load(), e.Metrics.Cancelled.Load(), e.Metrics.Failed.Load())
	}
	var samples []time.Duration
	for _, sample := range latencies {
		samples = append(samples, sample...)
	}
	slices.Sort(samples)
	if len(samples) == 0 {
		t.Fatal("no latency samples")
	}
	q := func(p int) time.Duration { return samples[(len(samples)-1)*p/100] }
	t.Log(fmt.Sprintf("sessions=%d throughput=%.1f/s p50=%v p95=%v p99=%v CPU=%.2fs heapMiB start/peak/after=%.2f/%.2f/%.2f allocB/session=%d goroutines=%d/%d/%d heartbeats=%d cancelled=%d failed=%d unexpected=%d drain=%v",
		total.Load(), float64(total.Load())/finish.Sub(start).Seconds(), q(50), q(95), q(99), cpu,
		float64(startMem.HeapInuse)/(1<<20), float64(peakMem.HeapInuse)/(1<<20), float64(endMem.HeapInuse)/(1<<20),
		(endMem.TotalAlloc-startMem.TotalAlloc)/uint64(total.Load()), initialG, peakG, endG, beats.Load(), cancels.Load(), failed.Load(), errorsFound.Load(), max(0, finish.Sub(deadline))))
}
