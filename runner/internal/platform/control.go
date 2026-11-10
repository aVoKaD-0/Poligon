package platform

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"time"

	pb "github.com/poligon/runner/contracts/controlv1"
	"github.com/poligon/runner/internal/execution"
	"go.opentelemetry.io/otel/propagation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type Control struct {
	RPC     pb.ControlServiceClient
	Token   string
	Timeout time.Duration
}

type TLSFiles struct {
	CA, Certificate, Key, ServerName string
}

func Dial(target string, local bool, files TLSFiles, maxPlan int) (*grpc.ClientConn, error) {
	var creds credentials.TransportCredentials
	if local && files.CA == "" {
		creds = insecure.NewCredentials()
	} else {
		cert, err := tls.LoadX509KeyPair(files.Certificate, files.Key)
		if err != nil {
			return nil, errors.New("cannot load Runner mTLS key pair")
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return nil, errors.New("invalid Runner certificate")
		}
		runner := false
		for _, uri := range leaf.URIs {
			if uri.String() == "spiffe://poligon/runner" {
				runner = true
			}
		}
		if !runner {
			return nil, errors.New("Runner certificate lacks required SPIFFE identity")
		}
		raw, err := os.ReadFile(files.CA)
		roots := x509.NewCertPool()
		if err != nil || !roots.AppendCertsFromPEM(raw) {
			return nil, errors.New("invalid Platform trust bundle")
		}
		creds = credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}, ServerName: files.ServerName,
		})
	}
	return grpc.NewClient(target, grpc.WithTransportCredentials(creds),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxPlan+(64<<10))), grpc.WithDisableRetry())
}

func (c *Control) call(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	if tp := carrier.Get("traceparent"); tp != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "traceparent", tp)
	}
	if c.Token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+c.Token)
	}
	return ctx, cancel
}

func (c *Control) Claim(ctx context.Context, req *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	return c.RPC.ClaimExecution(ctx, req)
}
func (c *Control) Heartbeat(ctx context.Context, req *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	return c.RPC.HeartbeatExecution(ctx, req)
}
func (c *Control) Plan(ctx context.Context, req *pb.PlanRequest) (*pb.LeaseResponse, error) {
	if err := execution.CheckLease(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	return c.RPC.GetRuntimePlan(ctx, req)
}
