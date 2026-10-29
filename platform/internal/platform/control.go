package platform

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	pb "github.com/poligon/platform/gen/controlv1"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type Control struct {
	pb.UnimplementedControlServiceServer
	App *App
}

func service(ctx context.Context) string { s, _ := ctx.Value(ctxKey("service")).(string); return s }
func (a *App) GRPCServer() (*grpc.Server, error) {
	opts := []grpc.ServerOption{grpc.MaxRecvMsgSize(1 << 20), grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		md, _ := metadata.FromIncomingContext(ctx)
		carrier := propagation.MapCarrier{}
		for k, values := range md {
			if len(values) > 0 {
				carrier[k] = values[0]
			}
		}
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
		ctx, span := otel.Tracer("poligon/platform").Start(ctx, info.FullMethod)
		defer span.End()
		identity := ""
		if a.Config.Env == "local" {
			md, _ := metadata.FromIncomingContext(ctx)
			v := md.Get("authorization")
			if len(v) == 1 {
				for s, t := range a.Config.ServiceTokens {
					if t != "" && subtle.ConstantTimeCompare([]byte(v[0]), []byte("Bearer "+t)) == 1 {
						identity = s
					}
				}
			}
		} else {
			if p, ok := peer.FromContext(ctx); ok {
				if auth, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(auth.State.VerifiedChains) > 0 {
					for _, uri := range auth.State.PeerCertificates[0].URIs {
						for _, s := range []string{"runner", "environment", "intelligence"} {
							if uri.String() == "spiffe://poligon/"+s {
								identity = s
							}
						}
					}
				}
			}
		}
		if identity == "" {
			return nil, status.Error(codes.Unauthenticated, "service identity required")
		}
		v, e := handler(context.WithValue(ctx, ctxKey("service"), identity), req)
		if e != nil {
			var api *APIError
			if errors.As(e, &api) {
				code := codes.Internal
				switch api.Status {
				case 400, 422:
					code = codes.InvalidArgument
				case 403:
					code = codes.PermissionDenied
				case 404:
					code = codes.NotFound
				case 409, 412:
					code = codes.FailedPrecondition
				case 429:
					code = codes.ResourceExhausted
				case 503:
					code = codes.Unavailable
				}
				return nil, status.Error(code, api.Code)
			}
			return nil, status.Error(codes.Internal, "INTERNAL_ERROR")
		}
		return v, e
	})}
	if a.Config.Env != "local" {
		cert, e := tls.LoadX509KeyPair(a.Config.TLSCert, a.Config.TLSKey)
		if e != nil {
			return nil, e
		}
		ca, e := os.ReadFile(a.Config.TLSCA)
		if e != nil {
			return nil, e
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("invalid GRPC_TLS_CA")
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert})))
	}
	server := grpc.NewServer(opts...)
	pb.RegisterControlServiceServer(server, &Control{App: a})
	return server, nil
}
func validateContext(c *pb.ExecutionContext) error {
	if c == nil || !validID(c.ProjectId) || !validID(c.RunId) || !validID(c.SessionId) || c.AttemptNo < 1 {
		return invalid("Invalid execution context")
	}
	return nil
}
func contextMap(c *pb.ExecutionContext) Object {
	return Object{"project_id": c.ProjectId, "run_id": c.RunId, "session_id": c.SessionId, "attempt_no": int(c.AttemptNo)}
}
func checkContext(ctx context.Context, q Queryer, c *pb.ExecutionContext) (Object, error) {
	if e := validateContext(c); e != nil {
		return nil, e
	}
	s, e := getSession(ctx, q, c.ProjectId, c.SessionId)
	if e != nil {
		return nil, e
	}
	if str(s, "run_id") != c.RunId {
		return nil, notFound()
	}
	return s, nil
}
func (c *Control) ClaimExecution(ctx context.Context, r *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	return c.lease(ctx, r, false, false)
}
func (c *Control) HeartbeatExecution(ctx context.Context, r *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	return c.lease(ctx, r, false, true)
}
func (c *Control) ClaimEvaluation(ctx context.Context, r *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	return c.lease(ctx, r, true, false)
}
func (c *Control) HeartbeatEvaluation(ctx context.Context, r *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	return c.lease(ctx, r, true, true)
}
func (c *Control) lease(ctx context.Context, r *pb.LeaseRequest, evaluation, heartbeat bool) (*pb.LeaseResponse, error) {
	a := c.App
	expected := "runner"
	if evaluation {
		expected = "intelligence"
	}
	if service(ctx) != expected {
		return nil, fail(403, "FORBIDDEN", "Wrong service")
	}
	if !validID(r.WorkerId) {
		return nil, invalid("worker_id must be UUID")
	}
	if heartbeat && !validID(r.LeaseId) {
		return nil, invalid("lease_id must be UUID")
	}
	out := &pb.LeaseResponse{}
	e := a.DB.Tx(ctx, func(tx pgx.Tx) error {
		s, e := checkContext(ctx, tx, r.Context)
		if e != nil {
			return e
		}
		p, run, sid := r.Context.ProjectId, r.Context.RunId, r.Context.SessionId
		n := int(r.Context.AttemptNo)
		out.Reason = "STALE_ATTEMPT"
		if integer(s, "current_attempt_no", 0) != n {
			return nil
		}
		var cancelled bool
		var runConcurrency, projectQuota int
		if e = tx.QueryRow(ctx, "SELECT r.cancel_requested,r.concurrency,p.concurrency FROM runs r JOIN projects p ON p.id=r.project_id WHERE r.id=$1", run).Scan(&cancelled, &runConcurrency, &projectQuota); e != nil {
			return e
		}
		table, where, args := "execution_attempts", "session_id=$1 AND attempt_no=$2", []any{sid, n}
		if evaluation {
			if !validID(r.EvaluationId) {
				return invalid("evaluation_id must be UUID")
			}
			table, where, args = "evaluation_jobs", "session_id=$1 AND attempt_no=$2 AND id=$3", []any{sid, n, r.EvaluationId}
		}
		v, e := oneJSON(ctx, tx, "SELECT to_jsonb(t) FROM "+table+" t WHERE "+where, args...)
		if e != nil {
			return e
		}
		if evaluation {
			if current, _ := v["current"].(bool); !current {
				return nil
			}
			if own, _ := v["cancel_requested"].(bool); own {
				cancelled = true
			}
			if str(v, "purpose") == "manual" {
				var runStatus string
				if e = tx.QueryRow(ctx, "SELECT status FROM runs WHERE id=$1", run).Scan(&runStatus); e != nil {
					return e
				}
				own, _ := v["cancel_requested"].(bool)
				if terminal(runStatus) {
					cancelled = own
				}
			}
		}
		if !evaluation && str(v, "dispatch_id") != r.DispatchId && !heartbeat {
			return nil
		}
		out.CancelRequested = cancelled
		expiry, _ := time.Parse(time.RFC3339Nano, str(v, "lease_expiry"))
		deadline, _ := time.Parse(time.RFC3339Nano, str(v, "deadline"))
		now := utc()
		active := str(v, "status") == "leased" || str(v, "status") == "running" || str(v, "status") == "preparing"
		if heartbeat {
			if !active || str(v, "worker_id") != r.WorkerId || str(v, "lease_id") != r.LeaseId || !expiry.After(now) || !deadline.After(now) {
				out.Reason = "LEASE_EXPIRED"
				return nil
			}
			expiry = now.Add(30 * time.Second)
			if expiry.After(deadline) {
				expiry = deadline
			}
			args = append(args, expiry)
			_, e = tx.Exec(ctx, "UPDATE "+table+" SET lease_expiry=$"+strconv.Itoa(len(args))+" WHERE "+where, args...)
			if e != nil {
				return e
			}
			out.Accepted = true
			out.Reason = "ACCEPTED"
			out.LeaseId = r.LeaseId
			out.LeaseExpiresAt = expiry.Format(time.RFC3339Nano)
			return nil
		}
		if cancelled {
			out.Reason = "CANCEL_REQUESTED"
			return nil
		}
		if active && expiry.After(now) {
			if str(v, "worker_id") != r.WorkerId {
				out.Reason = "ALREADY_CLAIMED"
				return nil
			}
			out.Accepted = true
			out.Reason = "ACCEPTED"
			out.LeaseId = str(v, "lease_id")
			out.LeaseExpiresAt = expiry.Format(time.RFC3339Nano)
			return c.attachPlan(ctx, tx, r.Context, out, !evaluation, r.EvaluationId)
		}
		if str(v, "status") != "queued" {
			return nil
		}
		var global, project, runningRun int
		if evaluation {
			e = tx.QueryRow(ctx, "SELECT count(*),count(*) FILTER(WHERE s.project_id=$1),count(*) FILTER(WHERE s.run_id=$2) FROM evaluation_jobs e JOIN sessions s ON s.id=e.session_id WHERE e.status='running'", p, run).Scan(&global, &project, &runningRun)
			if e != nil {
				return e
			}
			if global >= a.Config.GlobalEvaluations || project >= 5 {
				out.Reason = "CAPACITY_EXCEEDED"
				return nil
			}
		} else {
			e = tx.QueryRow(ctx, "SELECT count(*),count(*) FILTER(WHERE s.project_id=$1),count(*) FILTER(WHERE s.run_id=$2) FROM execution_attempts e JOIN sessions s ON s.id=e.session_id WHERE e.status IN('leased','preparing','running')", p, run).Scan(&global, &project, &runningRun)
			if e != nil {
				return e
			}
			if global >= a.Config.GlobalConcurrency || project >= projectQuota || runningRun >= runConcurrency {
				out.Reason = "CAPACITY_EXCEEDED"
				return nil
			}
		}
		duration := 330 * time.Second
		if evaluation {
			duration = 120 * time.Second
			var raw []byte
			if e = tx.QueryRow(ctx, "SELECT plan FROM execution_attempts WHERE session_id=$1 AND attempt_no=$2", sid, n).Scan(&raw); e != nil {
				return e
			}
			if str(sub(obj(raw), "scenario"), "modality") == "voice" {
				duration = 300 * time.Second
			}
		} else {
			duration = time.Duration(integer(sub(sub(v, "plan"), "limits"), "session_duration_ms", 300000)+30000) * time.Millisecond
		}
		deadline = now.Add(duration)
		expiry = now.Add(30 * time.Second)
		lease := ID()
		args = append(args, r.WorkerId, lease, expiry, deadline)
		size := len(args)
		next := "leased"
		if evaluation {
			next = "running"
		}
		_, e = tx.Exec(ctx, "UPDATE "+table+" SET status='"+next+"',worker_id=$"+strconv.Itoa(size-3)+",lease_id=$"+strconv.Itoa(size-2)+",lease_expiry=$"+strconv.Itoa(size-1)+",deadline=$"+strconv.Itoa(size)+" WHERE "+where, args...)
		if e != nil {
			return e
		}
		if !evaluation {
			if _, e = tx.Exec(ctx, "UPDATE sessions SET status='running' WHERE id=$1", sid); e != nil {
				return e
			}
			if e = aggregate(ctx, tx, run); e != nil {
				return e
			}
		}
		out.Accepted = true
		out.Reason = "ACCEPTED"
		out.LeaseId = lease
		out.LeaseExpiresAt = expiry.Format(time.RFC3339Nano)
		return c.attachPlan(ctx, tx, r.Context, out, !evaluation, r.EvaluationId)
	})
	return out, e
}
func (c *Control) attachPlan(ctx context.Context, q Queryer, ec *pb.ExecutionContext, out *pb.LeaseResponse, secrets bool, evaluationID string) error {
	var raw []byte
	e := q.QueryRow(ctx, "SELECT plan_ref::text,plan_sha256,plan FROM execution_attempts WHERE session_id=$1 AND attempt_no=$2", ec.SessionId, ec.AttemptNo).Scan(&out.RuntimePlanRef, &out.RuntimePlanSha256, &raw)
	if e != nil {
		return e
	}
	plan := obj(raw)
	out.RuntimePlanJson = raw
	if evaluationID != "" {
		if e = q.QueryRow(ctx, "SELECT v.definition,v.id::text,v.definition_sha256 FROM evaluation_jobs e JOIN versions v ON v.id=e.evaluator_version_id WHERE e.id=$1 AND e.session_id=$2 AND e.attempt_no=$3", evaluationID, ec.SessionId, ec.AttemptNo).Scan(&out.EvaluationDefinitionJson, &out.EvaluationVersionId, &out.EvaluationDefinitionSha256); e != nil {
			return e
		}
	}
	if secrets {
		ref := str(plan, "credential_revision_ref")
		if ref != "" {
			var cipher []byte
			e = q.QueryRow(ctx, "SELECT ciphertext FROM credentials WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL", ref, ec.ProjectId).Scan(&cipher)
			if e != nil {
				return fail(409, "CREDENTIAL_REVOKED", "Credential unavailable")
			}
			plain, e := decrypt(c.App.Config.MasterKey, cipher)
			if e != nil {
				return e
			}
			out.RuntimeCredentials = map[string]string{"agent": plain}
		}
	}
	return nil
}
func (c *Control) GetRuntimePlan(ctx context.Context, r *pb.PlanRequest) (*pb.LeaseResponse, error) {
	if service(ctx) != "runner" && service(ctx) != "intelligence" {
		return nil, fail(403, "FORBIDDEN", "Wrong service")
	}
	if _, e := checkContext(ctx, c.App.DB.Pool, r.Context); e != nil {
		return nil, e
	}
	if e := c.authorizeLease(ctx, r.Context, r.WorkerId, r.LeaseId, r.EvaluationId); e != nil {
		return nil, e
	}
	out := &pb.LeaseResponse{Accepted: true, Reason: "ACCEPTED"}
	if e := c.attachPlan(ctx, c.App.DB.Pool, r.Context, out, service(ctx) == "runner", r.EvaluationId); e != nil {
		return nil, e
	}
	if r.RuntimePlanRef != out.RuntimePlanRef || r.RuntimePlanSha256 != out.RuntimePlanSha256 {
		return nil, conflict("Runtime plan ref/hash mismatch")
	}
	return out, nil
}
func (c *Control) authorizeLease(ctx context.Context, ec *pb.ExecutionContext, worker, lease, evaluation string) error {
	if e := validateContext(ec); e != nil {
		return e
	}
	if !validID(worker) || !validID(lease) {
		return fail(403, "LEASE_REQUIRED", "Active lease required")
	}
	var ok bool
	var e error
	if service(ctx) == "intelligence" {
		if !validID(evaluation) {
			return invalid("evaluation_id must be UUID")
		}
		e = c.App.DB.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM evaluation_jobs e JOIN sessions s ON s.id=e.session_id WHERE e.id=$1 AND e.session_id=$2 AND e.attempt_no=$3 AND e.worker_id=$4 AND e.lease_id=$5 AND e.current AND e.status='running' AND e.lease_expiry>now() AND e.deadline>now() AND s.project_id=$6 AND s.run_id=$7)", evaluation, ec.SessionId, ec.AttemptNo, worker, lease, ec.ProjectId, ec.RunId).Scan(&ok)
	} else {
		e = c.App.DB.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM execution_attempts e JOIN sessions s ON s.id=e.session_id WHERE e.session_id=$1 AND e.attempt_no=$2 AND e.worker_id=$3 AND e.lease_id=$4 AND e.status IN('leased','preparing','running') AND e.lease_expiry>now() AND e.deadline>now() AND s.current_attempt_no=e.attempt_no AND s.project_id=$5 AND s.run_id=$6)", ec.SessionId, ec.AttemptNo, worker, lease, ec.ProjectId, ec.RunId).Scan(&ok)
	}
	if e != nil {
		return e
	}
	if !ok {
		return fail(403, "LEASE_REQUIRED", "Active lease required")
	}
	return nil
}
