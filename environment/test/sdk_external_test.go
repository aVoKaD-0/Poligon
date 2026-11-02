package test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A different Go module must be able to construct every SDK request using only
// public packages. Tests inside the environment module cannot enforce this rule.
func TestSDKBuildsFromExternalModule(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	version := ""
	for _, line := range strings.Split(string(module), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "go" {
			version = fields[1]
		}
	}
	dir := t.TempDir()
	goMod := "module poligon/runner\n\ngo " + version + "\n\nrequire poligon/environment v0.0.0\nreplace poligon/environment => " + strconv.Quote(root) + "\n"
	source := `package consumer
import (
    "context"
    "poligon/environment/pkg/api"
    "poligon/environment/pkg/client"
)
func UseSDK(ctx context.Context) {
    c := client.NewClient("http://localhost:8081", "s")
    c.InitSession(ctx, "s", []api.Client{{ID: "c"}}, []api.Slot{}, []api.FaultRule{{FaultType: api.FaultConflict}})
    c.CreateOrder(ctx, api.CreateOrderRequest{ClientID: "c", Address: "A", Date: "2026-10-30"}, "key")
    address := "B"
    c.UpdateOrder(ctx, "o", api.UpdateOrderRequest{Address: &address})
    c.CancelOrder(ctx, "o")
    c.Verify(ctx, api.VerificationCriteria{ClientID: "c", ExpectedStatus: "cancelled"})
    c.GetState(ctx)
    c.GetAuditLog(ctx)
    c.SearchSlots(ctx, "2026-10-30", "")
    c.AddFaultRule(ctx, api.FaultRule{FaultType: api.FaultTimeoutAfterSave})
    c.ResetSession(ctx)
}
`
	for name, content := range map[string]string{"go.mod": goMod, "consumer.go": source} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOPROXY=off", "GOTOOLCHAIN=local")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external SDK consumer failed to build: %v\n%s", err, output)
	}
}
