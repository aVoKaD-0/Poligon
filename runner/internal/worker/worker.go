package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/poligon/runner/internal/execution"
)

const (
	WorkStream  = "POLIGON_EXECUTION"
	EventStream = "POLIGON_EVENTS"
)

type Broker struct{ JS nats.JetStreamContext }

func (b Broker) Publish(ctx context.Context, subject, id string, body []byte) error {
	_, err := b.JS.Publish(subject, body, nats.Context(ctx), nats.MsgId(id), nats.ExpectStream(EventStream))
	return err
}

type Manager struct {
	Broker
	Durable                  string
	Slots, Pending           int
	AckWait, Progress, Drain time.Duration
	Log                      *slog.Logger
	Execute                  func(context.Context, execution.Job) execution.Disposition
	ready                    atomic.Bool
}

func (m *Manager) Ready() bool { return m.ready.Load() }

func (m *Manager) bind(ctx context.Context) (*nats.Subscription, error) {
	call, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info, err := m.JS.ConsumerInfo(WorkStream, m.Durable, nats.Context(call))
	if errors.Is(err, nats.ErrConsumerNotFound) {
		info, err = m.JS.AddConsumer(WorkStream, &nats.ConsumerConfig{
			Durable: m.Durable, FilterSubject: execution.CommandSubject, AckPolicy: nats.AckExplicitPolicy,
			DeliverPolicy: nats.DeliverAllPolicy, AckWait: m.AckWait, MaxAckPending: m.Pending,
			MaxDeliver: -1, MaxRequestBatch: 1,
		}, nats.Context(call))
	}
	if err != nil {
		return nil, err
	}
	c := info.Config
	if c.FilterSubject != execution.CommandSubject || c.AckPolicy != nats.AckExplicitPolicy ||
		c.DeliverPolicy != nats.DeliverAllPolicy || c.AckWait != m.AckWait ||
		c.MaxAckPending != m.Pending || c.MaxDeliver != -1 || c.DeliverSubject != "" {
		return nil, errors.New("incompatible Runner durable consumer")
	}
	// The subscribe context would also own the subscription lifetime. Use the
	// JetStream context's API timeout and explicitly unsubscribe after drain.
	sub, err := m.JS.PullSubscribe(execution.CommandSubject, m.Durable, nats.Bind(WorkStream, m.Durable), nats.ManualAck())
	if err != nil {
		return nil, err
	}
	if err = sub.SetPendingLimits(2*m.Slots, max(1<<20, 2*m.Slots*execution.MaxEvent)); err != nil {
		_ = sub.Unsubscribe()
		return nil, err
	}
	return sub, nil
}

func (m *Manager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		sub, err := m.bind(ctx)
		if err == nil {
			m.runBound(ctx, sub)
			_ = sub.Unsubscribe()
		} else if ctx.Err() == nil {
			m.Log.Warn("execution consumer unavailable")
		}
		if !pause(ctx, 2*time.Second) {
			return
		}
	}
}

func (m *Manager) runBound(ctx context.Context, sub *nats.Subscription) {
	pull, stopPull := context.WithCancel(ctx)
	defer stopPull()
	work, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	var group sync.WaitGroup
	var lastPullWarning atomic.Int64
	m.ready.Store(true)
	for range m.Slots {
		group.Go(func() {
			for pull.Err() == nil {
				fetch, stop := context.WithTimeout(pull, time.Second)
				msgs, err := sub.Fetch(1, nats.Context(fetch))
				stop()
				if err != nil {
					if errors.Is(err, nats.ErrConsumerDeleted) || errors.Is(err, nats.ErrNoResponders) || errors.Is(err, nats.ErrBadSubscription) {
						stopPull()
						return
					}
					if pull.Err() == nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, nats.ErrTimeout) {
						now, previous := time.Now().Unix(), lastPullWarning.Load()
						if now-previous >= 5 && lastPullWarning.CompareAndSwap(previous, now) {
							m.Log.Warn("pull failed", "error_type", fmt.Sprintf("%T", err))
						}
						if !pause(pull, time.Second) {
							return
						}
					}
					continue
				}
				for _, msg := range msgs {
					if pull.Err() != nil {
						_ = msg.Nak()
						return
					}
					m.handle(work, msg)
				}
			}
		})
	}
	<-pull.Done()
	m.ready.Store(false)
	drained := make(chan struct{})
	go func() { group.Wait(); close(drained) }()
	timer := time.NewTimer(m.Drain)
	defer timer.Stop()
	select {
	case <-drained:
	case <-timer.C:
		cancel()
		<-drained
	}
}

func (m *Manager) handle(ctx context.Context, msg *nats.Msg) {
	job, err := execution.DecodeJob(msg.Subject, msg.Data)
	if err != nil {
		m.Log.Warn("invalid execution command")
		if msg.Term() != nil {
			m.Log.Warn("command rejection not sent")
		}
		return
	}
	// The broker's AckWait and Platform's execution lease are separate clocks.
	progress, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(m.Progress)
		defer ticker.Stop()
		for {
			select {
			case <-progress.Done():
				return
			case <-ticker.C:
				if msg.InProgress() != nil {
					m.Log.Warn("broker progress not sent", "session_id", job.SessionID)
				}
			}
		}
	}()
	result := m.Execute(ctx, job)
	stop()
	<-done
	if result == execution.Ack {
		ack, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if msg.AckSync(nats.Context(ack)) != nil {
			m.Log.Warn("command acknowledgement unconfirmed", "session_id", job.SessionID)
		}
	} else if msg.NakWithDelay(3*time.Second+time.Duration(rand.IntN(500))*time.Millisecond) != nil {
		m.Log.Warn("command retry not sent", "session_id", job.SessionID)
	}
}

func pause(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
