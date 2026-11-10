package execution

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/propagation"
)

type Blob struct {
	Kind, MIME, SHA string
	Size            int64
	Data            io.ReadSeeker
}

type journal struct {
	f    *os.File
	out  *bufio.Writer
	hash hash.Hash
	size int64
}

func openJournal(dir string) (*journal, error) {
	f, err := os.CreateTemp(dir, "runner-*.ndjson")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	return &journal{f: f, out: bufio.NewWriter(io.MultiWriter(f, h)), hash: h}, nil
}

func (j *journal) close() {
	j.f.Close()
	os.Remove(j.f.Name())
}

func (j *journal) blob(kind string) (Blob, error) {
	if err := j.out.Flush(); err != nil {
		return Blob{}, err
	}
	if _, err := j.f.Seek(0, io.SeekStart); err != nil {
		return Blob{}, err
	}
	return Blob{Kind: kind, MIME: "application/x-ndjson", SHA: hex.EncodeToString(j.hash.Sum(nil)), Size: j.size, Data: j.f}, nil
}

type record struct {
	Scope
	Schema  int       `json:"schema_version"`
	ID      string    `json:"producer_event_id"`
	Seq     int       `json:"session_seq"`
	Kind    string    `json:"event_kind"`
	At      time.Time `json:"occurred_at"`
	Payload any       `json:"payload"`
}

type Publisher interface {
	Publish(ctx context.Context, subject, id string, data []byte) error
}

type events struct {
	job         Job
	broker      Publisher
	seq         int
	traceparent string
}

func newEvents(ctx context.Context, job Job, broker Publisher) *events {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return &events{job: job, broker: broker, traceparent: carrier.Get("traceparent")}
}

func (e *events) publish(ctx context.Context, kind, id string, at time.Time, payload any, attempts int) error {
	msg := struct {
		Scope
		Schema      int       `json:"schema_version"`
		ID          string    `json:"message_id"`
		ProducerID  string    `json:"producer_event_id"`
		Producer    string    `json:"producer"`
		Type        string    `json:"type"`
		At          time.Time `json:"occurred_at"`
		Correlation string    `json:"correlation_id"`
		Causation   string    `json:"causation_id"`
		Traceparent string    `json:"traceparent"`
		Payload     any       `json:"payload"`
	}{e.job.Scope, 1, id, id, "runner", kind, at, e.job.RunID, e.job.MessageID, e.traceparent, payload}
	raw, err := MarshalJSON(msg)
	if err != nil {
		return err
	}
	if len(raw) > MaxEvent {
		return ErrLimit
	}
	for i := 0; i < attempts; i++ {
		if err := CheckLease(ctx); err != nil {
			return err
		}
		pub, cancel := context.WithTimeout(ctx, time.Second)
		err = e.broker.Publish(pub, "poligon.evt."+kind, id, raw)
		cancel()
		if err == nil {
			return nil
		}
		if i+1 < attempts {
			timer := time.NewTimer(time.Duration(i+1) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return context.Cause(ctx)
			case <-timer.C:
			}
		}
	}
	return err
}

func (e *events) state(ctx context.Context, kind string, payload map[string]any) error {
	e.seq++
	payload["session_seq"] = e.seq
	return e.publish(ctx, kind, uuid.NewString(), time.Now().UTC(), payload, 3)
}

type recorder struct {
	events              *events
	conversation, tools *journal
	secret              string
	limit               int64
	missed              int
}

func (r *recorder) append(ctx context.Context, j *journal, kind string, payload any) error {
	r.events.seq++
	now := time.Now().UTC()
	entry := record{r.events.job.Scope, 1, uuid.NewString(), r.events.seq, kind, now, payload}
	if r.secret != "" {
		// Redact the payload without altering the event's scope or identity.
		raw, err := MarshalJSON(payload)
		if err != nil {
			return err
		}
		entry.Payload = json.RawMessage(raw)
		// An escape can conceal the credential from a literal byte search.
		if bytes.IndexByte(raw, '\\') >= 0 || bytes.Contains(raw, []byte(r.secret)) {
			var parsed any
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&parsed); err != nil {
				return err
			}
			entry.Payload = redactPayload(kind, parsed, r.secret)
		}
	}
	// Keep normal JSON strings compact. Platform's projection reads lines of
	// at most 64 KiB, independently of the broker envelope limit.
	raw, err := MarshalJSON(entry)
	if err != nil {
		return err
	}
	if len(raw) >= MaxEvent || r.conversation.size+r.tools.size+int64(len(raw))+1 > r.limit {
		return ErrLimit
	}
	if _, err := j.out.Write(raw); err != nil {
		return err
	}
	if err := j.out.WriteByte('\n'); err != nil {
		return err
	}
	j.size += int64(len(raw)) + 1
	if ctx.Err() == nil {
		// Reserve envelope overhead. Larger records are recovered from the journal.
		if len(raw) > MaxEvent-1024 || r.events.publish(ctx, "session.progress.v1", entry.ID, now, json.RawMessage(raw), 1) != nil {
			r.missed++
		}
	}
	return nil
}

func redactPayload(kind string, payload any, secret string) any {
	fields, ok := payload.(map[string]any)
	if !ok {
		return redact(payload, secret)
	}
	var external []string
	switch kind {
	case "client.message":
		external = []string{"text"}
	case "agent.message":
		external = []string{"messages", "cost_minor"}
	case "tool.request", "tool.skipped":
		if kind == "tool.skipped" {
			fields, _ = fields["call"].(map[string]any)
		}
		external = []string{"id", "name", "arguments", "idempotency_key"}
	case "tool.result":
		fields, _ = fields["result"].(map[string]any)
		external = []string{"id", "body"}
	case "session.ended", "session.error":
		return payload
	default:
		return redact(payload, secret)
	}
	// Record field names, counters, statuses, and timestamps are Runner's
	// metadata. Only fields containing externally supplied data need scrubbing.
	for _, key := range external {
		if v, exists := fields[key]; exists {
			fields[key] = redact(v, secret)
		}
	}
	return payload
}

func redact(v any, secret string) any {
	switch v := v.(type) {
	case string:
		return redactText(v, secret)
	case json.Number:
		if strings.Contains(string(v), secret) {
			return nil
		}
	case map[string]any:
		result := make(map[string]any, len(v))
		var hiddenKeys []string
		for k, value := range v {
			if strings.Contains(k, secret) {
				hiddenKeys = append(hiddenKeys, k)
			} else {
				result[k] = redact(value, secret)
			}
		}
		sort.Strings(hiddenKeys)
		var suffix int
		for _, k := range hiddenKeys {
			base := redactText(k, secret)
			key := base
			for {
				if _, exists := result[key]; !exists {
					break
				}
				suffix++
				// Non-ASCII digits keep a numeric credential out of the key.
				index := strings.Map(func(r rune) rune { return '０' + r - '0' }, strconv.Itoa(suffix))
				key = base + "＃" + index
			}
			result[key] = redact(v[k], secret)
		}
		return result
	case []any:
		for i := range v {
			v[i] = redact(v[i], secret)
		}
	}
	return v
}

func redactText(text, secret string) string {
	const marker = "[REDACTED]"
	if len(secret) < len(marker) {
		for _, symbol := range "*#~!^?=@+" {
			if !strings.ContainsRune(secret, symbol) {
				return strings.ReplaceAll(text, secret, string(symbol))
			}
		}
	}
	text = strings.ReplaceAll(text, secret, marker)
	// For short credentials or overlapping matches at the marker boundary,
	// use a separator that cannot occur in an HTTP bearer token.
	return strings.ReplaceAll(text, secret, "■")
}

func journalError(err error) string {
	if errors.Is(err, ErrLimit) {
		return "EVIDENCE_LIMIT_EXCEEDED"
	}
	return "INFRASTRUCTURE_ERROR"
}
