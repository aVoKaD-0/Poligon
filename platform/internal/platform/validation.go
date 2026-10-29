package platform

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/poligon/platform/contracts"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
)

var schemas = func() map[string]*jsonschema.Schema {
	out := map[string]*jsonschema.Schema{}
	for _, name := range []string{"agent", "scenario", "artifact-ref", "execution-manifest", "assessment"} {
		b, e := contracts.Files.ReadFile("schemas/" + name + ".json")
		if e != nil {
			panic(e)
		}
		var v any
		if e = json.Unmarshal(b, &v); e != nil {
			panic(e)
		}
		c := jsonschema.NewCompiler()
		if e = c.AddResource(name+".json", v); e != nil {
			panic(e)
		}
		s, e := c.Compile(name + ".json")
		if e != nil {
			panic(e)
		}
		out[name] = s
	}
	return out
}()

func ValidateDefinition(kind string, d Object) error {
	s := schemas[kind]
	if s == nil {
		return invalid("Unsupported definition kind")
	}
	var normalized any
	_ = json.Unmarshal(encode(d), &normalized)
	if e := s.Validate(normalized); e != nil {
		return &APIError{422, "VALIDATION_ERROR", "Definition не соответствует JSON Schema", Object{"validation": e.Error()}}
	}
	if kind == "scenario" {
		if _, e := time.Parse(time.RFC3339, str(d, "virtual_clock")); e != nil {
			return invalid("virtual_clock must be RFC3339")
		}
		if _, e := time.LoadLocation(str(d, "business_timezone")); e != nil {
			return invalid("Unknown business_timezone")
		}
		if str(d, "modality") == "voice" && d["voice_actions"] == nil {
			return invalid("voice_actions required for voice")
		}
	}
	return nil
}
func ValidateAssessment(d Object) error {
	if integer(d, "schema_version", 0) != 1 || !validID(str(d, "evaluation_id")) || !validID(str(d, "evaluator_version_id")) {
		return invalid("Invalid assessment identity")
	}
	validity := str(d, "simulation_validity")
	if validity != "valid" && validity != "invalid" && validity != "unknown" {
		return invalid("Unknown simulation_validity")
	}
	verdict := str(d, "verdict")
	if verdict != "pass" && verdict != "fail" && verdict != "inconclusive" {
		return invalid("Unknown verdict")
	}
	rows, ok := d["assertions"].([]any)
	if !ok {
		return invalid("assertions array required")
	}
	applicable, failed, uncertain := 0, false, false
	for _, raw := range rows {
		assertion, ok := raw.(map[string]any)
		if !ok {
			return invalid("Invalid assertion")
		}
		status := str(assertion, "status")
		switch status {
		case "pass", "fail", "inconclusive", "error", "not_applicable":
		default:
			return invalid("Unknown assertion status")
		}
		required, ok := assertion["required"].(bool)
		if !ok {
			return invalid("Assertion required flag must be explicit")
		}
		if status == "not_applicable" && assertion["applicability"] == nil {
			return invalid("not_applicable requires applicability evidence")
		}
		if !required || status == "not_applicable" {
			continue
		}
		applicable++
		failed = failed || status == "fail"
		uncertain = uncertain || status == "inconclusive" || status == "error"
	}
	expected := "pass"
	if applicable == 0 || uncertain {
		expected = "inconclusive"
	}
	if failed {
		expected = "fail"
	}
	if validity != "valid" {
		expected = "inconclusive"
	}
	if verdict != expected {
		return invalid("Assessment verdict contradicts required assertions or simulation validity")
	}
	return nil
}

// Validator RPCs are external contracts. This client never reads provider tables.
// The proposed Struct wire contract is documented in contracts/proto/validation.proto.
type RemoteValidators struct {
	Environment, Intelligence *grpc.ClientConn
	Token                     string
}

func ValidatorConnection(c Config, address string) (*grpc.ClientConn, error) {
	if address == "" {
		return nil, errors.New("validator address required")
	}
	var creds credentials.TransportCredentials = insecure.NewCredentials()
	if c.Env != "local" {
		cert, e := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if e != nil {
			return nil, e
		}
		ca, e := os.ReadFile(c.TLSCA)
		if e != nil {
			return nil, e
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("invalid CA")
		}
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}})
	}
	return grpc.NewClient(address, grpc.WithTransportCredentials(creds))
}
func (v *RemoteValidators) call(ctx context.Context, conn *grpc.ClientConn, path string, input Object) (Object, error) {
	if conn == nil {
		return nil, fail(503, "DEPENDENCY_UNAVAILABLE", "Validator unavailable")
	}
	ctx, c := context.WithTimeout(ctx, 3*time.Second)
	defer c()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+v.Token)
	in, e := structpb.NewStruct(protoObject(input))
	if e != nil {
		return nil, invalid("Invalid validator request")
	}
	out := &structpb.Struct{}
	if e = conn.Invoke(ctx, path, in, out); e != nil {
		return nil, fail(503, "DEPENDENCY_UNAVAILABLE", "Validator unavailable")
	}
	o := out.AsMap()
	if rows, ok := o["errors"].([]any); ok && len(rows) > 0 {
		return nil, &APIError{422, "VALIDATION_ERROR", "Внешний validator отклонил definition", Object{"errors": rows}}
	}
	return o, nil
}
func (v *RemoteValidators) ValidateScenario(ctx context.Context, d Object) error {
	if _, e := v.call(ctx, v.Environment, "/poligon.environment.v1.EnvironmentService/ValidateTemplate", d); e != nil {
		return e
	}
	_, e := v.call(ctx, v.Intelligence, "/poligon.intelligence.v1.IntelligenceService/ValidateScenario", d)
	return e
}
func (v *RemoteValidators) CheckCompatibility(ctx context.Context, agent Object, scenarios []Object, evaluator Object) error {
	for _, s := range scenarios {
		d := sub(s, "definition")
		if e := v.ValidateScenario(ctx, d); e != nil {
			return e
		}
		for _, provider := range []struct {
			conn              *grpc.ClientConn
			path, field, kind string
		}{{v.Environment, "/poligon.environment.v1.EnvironmentService/GetCapabilities", "environment_version_id", "environment"}, {v.Intelligence, "/poligon.intelligence.v1.IntelligenceService/GetCapabilities", "simulator_version_id", "simulator"}, {v.Intelligence, "/poligon.intelligence.v1.IntelligenceService/GetCapabilities", "evaluator_version_id", "evaluator"}} {
			id := str(d, provider.field)
			if provider.kind == "evaluator" {
				id = str(evaluator, "id")
			}
			o, e := v.call(ctx, provider.conn, provider.path, Object{"version_id": id, "kind": provider.kind, "modality": d["modality"], "scenario": d})
			if e != nil {
				return e
			}
			supported, ok := o["supported"].(bool)
			if !ok || !supported {
				return invalid("Capability is not supported: " + provider.kind)
			}
		}
	}
	return nil
}
func (a *App) SeedCatalog(ctx context.Context) error {
	b, e := contracts.Files.ReadFile("catalog/builtin.v1.json")
	if e != nil {
		return e
	}
	var rows []Object
	if e = json.Unmarshal(b, &rows); e != nil {
		return e
	}
	return a.DB.Tx(ctx, func(tx pgx.Tx) error {
		for _, v := range rows {
			_, h, e := canonical(v["definition"])
			if e != nil {
				return e
			}
			var old string
			e = tx.QueryRow(ctx, "SELECT definition_sha256 FROM versions WHERE id=$1", str(v, "id")).Scan(&old)
			if e == nil {
				if old != h {
					return conflict("Catalog definition changed under existing id")
				}
				continue
			}
			if !errors.Is(e, pgx.ErrNoRows) {
				return e
			}
			_, e = tx.Exec(ctx, "INSERT INTO versions(id,kind,version_number,status,definition,definition_sha256,published_at) VALUES($1,$2,1,'published',$3,$4,now())", str(v, "id"), str(v, "kind"), encode(v["definition"]), h)
			if e != nil {
				return e
			}
		}
		return nil
	})
}

func protoObject(o Object) Object { var v Object; _ = json.Unmarshal(encode(o), &v); return v }

func validateArtifactSchema(kind string, v Object) error {
	var normalized any
	_ = json.Unmarshal(encode(v), &normalized)
	if e := schemas[kind].Validate(normalized); e != nil {
		return &APIError{422, "INVALID_ARTIFACT", "Артефакт не соответствует schema v1", Object{"validation": e.Error()}}
	}
	return nil
}
