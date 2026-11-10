package platform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/poligon/runner/contracts/controlv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type mtlsFixture struct {
	pb.UnimplementedControlServiceServer
}

func (mtlsFixture) ClaimExecution(ctx context.Context, _ *pb.LeaseRequest) (*pb.LeaseResponse, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing peer")
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.PeerCertificates[0].URIs) != 1 ||
		info.State.PeerCertificates[0].URIs[0].String() != "spiffe://poligon/runner" {
		return nil, status.Error(codes.Unauthenticated, "wrong service identity")
	}
	return &pb.LeaseResponse{Accepted: true}, nil
}

func TestControlMutualTLS(t *testing.T) {
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "local CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, public, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, usage x509.ExtKeyUsage, dns []string, uris []*url.URL) ([]byte, []byte) {
		t.Helper()
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Minute),
			NotAfter: now.Add(time.Hour), DNSNames: dns, URIs: uris, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		cert, err := x509.CreateCertificate(rand.Reader, leaf, ca, public, key)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})
	}
	root := t.TempDir()
	write := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	uri, _ := url.Parse("spiffe://poligon/runner")
	runner, runnerKey := issue(2, x509.ExtKeyUsageClientAuth, nil, []*url.URL{uri})
	platform, platformKey := issue(3, x509.ExtKeyUsageServerAuth, []string{"platform.test"}, nil)
	cert, err := tls.X509KeyPair(platform, platformKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
	})))
	pb.RegisterControlServiceServer(srv, mtlsFixture{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	defer srv.Stop()
	files := TLSFiles{CA: write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		Certificate: write("runner.pem", runner), Key: write("runner-key.pem", runnerKey), ServerName: "platform.test"}
	conn, err := Dial(lis.Addr().String(), false, files, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctl := Control{RPC: pb.NewControlServiceClient(conn), Timeout: time.Second}
	out, err := ctl.Claim(t.Context(), &pb.LeaseRequest{})
	if err != nil || !out.GetAccepted() {
		t.Fatalf("mTLS claim failed: %v", err)
	}
	files.Certificate, files.Key = write("platform.pem", platform), write("platform-key.pem", platformKey)
	if wrong, err := Dial(lis.Addr().String(), false, files, 1<<20); err == nil {
		wrong.Close()
		t.Fatal("client certificate without Runner SPIFFE identity accepted")
	}
}
