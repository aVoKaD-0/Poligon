package worker

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/poligon/runner/internal/execution"
)

func startBroker(t *testing.T) (*nats.Conn, nats.JetStreamContext) {
	t.Helper()
	s := startServer(t, server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoSigs: true})
	c, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, createStreams(t, c)
}

func startServer(t *testing.T, opts server.Options) *server.Server {
	t.Helper()
	s, err := server.NewServer(&opts)
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	t.Cleanup(func() { s.Shutdown(); s.WaitForShutdown() })
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("JetStream did not start")
	}
	return s
}

func createStreams(t *testing.T, c *nats.Conn) nats.JetStreamContext {
	t.Helper()
	js, err := c.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []nats.StreamConfig{
		{Name: WorkStream, Subjects: []string{"poligon.cmd.session.>"}, Retention: nats.WorkQueuePolicy, Storage: nats.FileStorage},
		{Name: EventStream, Subjects: []string{"poligon.evt.>"}, Storage: nats.FileStorage},
	} {
		if _, err := js.AddStream(&cfg); err != nil {
			t.Fatal(err)
		}
	}
	return js
}

func TestJetStreamReconnect(t *testing.T) {
	opts := server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoSigs: true}
	s := startServer(t, opts)
	disconnected, reconnected := make(chan struct{}, 1), make(chan struct{}, 1)
	c, err := nats.Connect(s.ClientURL(), nats.ReconnectWait(50*time.Millisecond), nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(*nats.Conn, error) {
			select {
			case disconnected <- struct{}{}:
			default:
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) {
			select {
			case reconnected <- struct{}{}:
			default:
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	js := createStreams(t, c)
	jobs := make(chan string, 2)
	m := &Manager{
		Broker: Broker{js}, Durable: "runner-reconnect", Slots: 1, Pending: 2,
		AckWait: 700 * time.Millisecond, Progress: 70 * time.Millisecond, Drain: time.Second,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Execute: func(_ context.Context, job execution.Job) execution.Disposition {
			jobs <- job.MessageID
			return execution.Ack
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	wait := func(signal <-chan struct{}, message string) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(6 * time.Second):
			t.Fatal(message)
		}
	}
	waitJob := func(want string) {
		t.Helper()
		select {
		case got := <-jobs:
			if got != want {
				t.Fatalf("received %s, want %s", got, want)
			}
		case <-time.After(6 * time.Second):
			t.Fatal("execution command not received")
		}
		until := time.Now().Add(5 * time.Second)
		for {
			info, err := js.StreamInfo(WorkStream)
			if err != nil {
				t.Fatal(err)
			}
			if info.State.Msgs == 0 {
				return
			}
			if time.Now().After(until) {
				t.Fatal("execution acknowledgement not applied")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitJob(publishJob(t, js))
	opts.Port = s.Addr().(*net.TCPAddr).Port
	s.Shutdown()
	s.WaitForShutdown()
	wait(disconnected, "client did not observe broker shutdown")
	startServer(t, opts)
	wait(reconnected, "client did not reconnect")
	waitJob(publishJob(t, js))
}

func TestJetStreamBoundedConcurrencyAndAcknowledgements(t *testing.T) {
	_, js := startBroker(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var active, peak, executed atomic.Int32
	var seen sync.Map
	m := &Manager{
		Broker: Broker{js}, Durable: "runner-test", Slots: 20, Pending: 20,
		AckWait: 700 * time.Millisecond, Progress: 70 * time.Millisecond, Drain: time.Second, Log: logger,
	}
	m.Execute = func(ctx context.Context, j execution.Job) execution.Disposition {
		if _, duplicate := seen.LoadOrStore(j.SessionID, true); duplicate {
			t.Error("business work executed twice")
		}
		n := active.Add(1)
		defer active.Add(-1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		time.Sleep(900 * time.Millisecond)
		if err := m.Publish(ctx, "poligon.evt.session.executed.v1", j.MessageID, []byte("{}")); err != nil {
			t.Error(err)
			return execution.Retry
		}
		executed.Add(1)
		return execution.Ack
	}
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for range 23 {
		publishJob(t, js)
	}
	if _, err := js.Publish(execution.CommandSubject, []byte("{bad")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		info, err := js.StreamInfo(WorkStream)
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Msgs == 0 {
			break
		}
		if time.Now().After(deadline) {
			ci, _ := js.ConsumerInfo(WorkStream, m.Durable)
			t.Fatalf("pending jobs: %d, executed: %d, peak: %d, consumer: %+v", info.State.Msgs, executed.Load(), peak.Load(), ci)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if executed.Load() != 23 || peak.Load() != 20 {
		t.Fatalf("executed=%d peak=%d", executed.Load(), peak.Load())
	}
	info, err := js.StreamInfo(EventStream)
	if err != nil || info.State.Msgs != 23 {
		t.Fatalf("terminal messages: %+v, %v", info, err)
	}
}

func publishJob(t *testing.T, js nats.JetStreamContext) string {
	t.Helper()
	j := execution.Job{
		Scope:  execution.Scope{ProjectID: uuid.NewString(), RunID: uuid.NewString(), SessionID: uuid.NewString(), Attempt: 1},
		Schema: 1, MessageID: uuid.NewString(), ProducerEventID: uuid.NewString(), Type: "session.execute.v1", Producer: "platform", OccurredAt: time.Now(),
	}
	j.Payload.Dispatch = uuid.NewString()
	j.Payload.PlanRef = uuid.NewString()
	j.Payload.PlanHash = strings.Repeat("a", 64)
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish(execution.CommandSubject, b); err != nil {
		t.Fatal(err)
	}
	return j.MessageID
}

func TestJetStreamDurableAfterDrain(t *testing.T) {
	_, js := startBroker(t)
	publishJob(t, js)
	start := func(execute func(context.Context, execution.Job) execution.Disposition) (context.CancelFunc, <-chan struct{}) {
		ctx, cancel := context.WithCancel(t.Context())
		m := &Manager{
			Broker: Broker{js}, Durable: "runner-restart", Slots: 1, Pending: 1,
			AckWait: 700 * time.Millisecond, Progress: 70 * time.Millisecond, Drain: 50 * time.Millisecond,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Execute: execute,
		}
		done := make(chan struct{})
		go func() { defer close(done); m.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
		return cancel, done
	}
	first := make(chan string, 1)
	stop, done := start(func(ctx context.Context, j execution.Job) execution.Disposition {
		first <- j.MessageID
		<-ctx.Done()
		return execution.Retry
	})
	var id string
	select {
	case id = <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("first worker did not claim the command")
	}
	stop()
	<-done
	second := make(chan string, 1)
	start(func(_ context.Context, j execution.Job) execution.Disposition {
		second <- j.MessageID
		return execution.Ack
	})
	select {
	case got := <-second:
		if got != id {
			t.Fatal("durable did not redeliver the same command")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("command lost on Runner restart")
	}
}

func TestJetStreamRebindsDeletedDurable(t *testing.T) {
	_, js := startBroker(t)
	ctx, cancel := context.WithCancel(t.Context())
	received := make(chan string, 2)
	m := &Manager{
		Broker: Broker{js}, Durable: "runner-rebind", Slots: 2, Pending: 2,
		AckWait: time.Second, Progress: 100 * time.Millisecond, Drain: time.Second,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Execute: func(_ context.Context, job execution.Job) execution.Disposition {
			received <- job.MessageID
			return execution.Ack
		},
	}
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitJob := func(id string) {
		t.Helper()
		select {
		case got := <-received:
			if got != id {
				t.Fatalf("received %s, expected %s", got, id)
			}
		case <-time.After(8 * time.Second):
			t.Fatal("consumer did not receive the command")
		}
	}
	waitJob(publishJob(t, js))
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, err := js.StreamInfo(WorkStream)
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Msgs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("warm-up command was not acknowledged")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := js.DeleteConsumer(WorkStream, m.Durable); err != nil {
		t.Fatal(err)
	}
	waitJob(publishJob(t, js))
}

func TestPlatformBrokerACL(t *testing.T) {
	root := os.Getenv("POLIGON_PLATFORM_TEST_DIR")
	if root == "" {
		t.Skip("set POLIGON_PLATFORM_TEST_DIR to use Platform NATS ACL")
	}
	t.Setenv("NATS_PLATFORM_PASSWORD", "platform-test-password")
	t.Setenv("NATS_RUNNER_PASSWORD", "runner-test-password")
	t.Setenv("NATS_INTELLIGENCE_PASSWORD", "intelligence-test-password")
	opts, err := server.ProcessConfigFile(filepath.Join(root, "deploy", "nats.conf"))
	if err != nil {
		t.Fatal(err)
	}
	opts.Host, opts.Port, opts.HTTPPort, opts.StoreDir, opts.NoSigs = "127.0.0.1", -1, 0, t.TempDir(), true
	s := startServer(t, *opts)
	platform, err := nats.Connect(s.ClientURL(), nats.UserInfo("platform", os.Getenv("NATS_PLATFORM_PASSWORD")))
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	producer := createStreams(t, platform)
	runner, err := nats.Connect(s.ClientURL(), nats.UserInfo("runner", os.Getenv("NATS_RUNNER_PASSWORD")))
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	js, err := runner.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Broker: Broker{js}, Durable: "runner-acl", Slots: 1, Pending: 1, AckWait: time.Second}
	sub, err := m.bind(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	id := publishJob(t, producer)
	msgs, err := sub.Fetch(1, nats.MaxWait(2*time.Second))
	if err != nil || len(msgs) != 1 {
		t.Fatalf("authorized pull failed: %v", err)
	}
	if err = m.Publish(t.Context(), "poligon.evt.session.failed.v1", id, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	ack, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err = msgs[0].AckSync(nats.Context(ack)); err != nil {
		t.Fatal(err)
	}
}
