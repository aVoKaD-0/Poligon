package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"math"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"poligon/environment/contracts"
	env "poligon/environment/gen/environmentv1"
	"poligon/environment/pkg/api"
)

// This is the Environment entry in Platform's pinned, registrable builtin catalog.
const versionID = "a0000000-0000-4000-8000-000000000001"

func (s *Server) GRPC() *grpc.Server {
	server := grpc.NewServer(grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		var token string
		switch info.FullMethod {
		case env.EnvironmentService_GetCapabilities_FullMethodName, env.EnvironmentService_ValidateTemplate_FullMethodName:
			token = s.platform
		case env.EnvironmentService_FinalizeAttempt_FullMethodName:
			token = s.runner
		default:
			return nil, status.Error(codes.Unimplemented, "Unknown Environment method")
		}
		headers := metadata.ValueFromIncomingContext(ctx, "authorization")
		if len(headers) != 1 || !authorized(headers[0], token) {
			return nil, status.Error(codes.Unauthenticated, "Service token required for method")
		}
		return next(ctx, req)
	}))
	env.RegisterEnvironmentServiceServer(server, s)
	return server
}

func safeNumbers(v any) bool {
	switch n := v.(type) {
	case float64:
		return !math.IsNaN(n) && !math.IsInf(n, 0) && (math.Trunc(n) != n || math.Abs(n) <= 9007199254740991)
	case []any:
		for _, item := range n {
			if !safeNumbers(item) {
				return false
			}
		}
	case map[string]any:
		for _, item := range n {
			if !safeNumbers(item) {
				return false
			}
		}
	}
	return true
}

func decodeFields(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return strictJSON(bytes.NewReader(b), out)
}

type issues []any

func (i *issues) add(path, code, message string) {
	*i = append(*i, map[string]any{"path": path, "code": code, "message": message})
}

func validate(d map[string]any) issues {
	errs := issues{}
	if d["schema_version"] != float64(1) {
		errs.add("schema_version", "unsupported", "Expected schema_version 1")
	}
	if !safeNumbers(d) {
		errs.add("$", "unsafe_number", "Integer exceeds exact Struct representation")
	}
	for _, field := range []struct{ key, value string }{{"template_id", "field-service"}, {"template_version", "v1"}, {"environment_version_id", versionID}, {"modality", "text"}} {
		if d[field.key] != field.value {
			errs.add(field.key, "unsupported", "Expected "+field.value)
		}
	}
	fixture, ok := d["initial_fixture"].(map[string]any)
	var f initial
	if !ok {
		errs.add("initial_fixture", "invalid_type", "Expected an object")
	} else if _, orders := fixture["orders"]; orders {
		errs.add("initial_fixture.orders", "unsupported", "Initial orders are unsupported")
	} else if err := decodeFields(fixture, &f); err != nil {
		errs.add("initial_fixture", "invalid_fixture", err.Error())
	} else if err := f.validate(); err != nil {
		errs.add("initial_fixture", "invalid_fixture", err.Error())
	}
	rules, ok := d["fault_rules"].([]any)
	var decoded []api.FaultRule
	if !ok {
		errs.add("fault_rules", "invalid_type", "Expected an array")
	} else if err := decodeFields(rules, &decoded); err != nil {
		errs.add("fault_rules", "invalid_rule", err.Error())
	} else if err := validateRules(decoded); err != nil {
		errs.add("fault_rules", "invalid_rule", err.Error())
	}
	return errs
}

func (s *Server) ValidateTemplate(_ context.Context, r *structpb.Struct) (*structpb.Struct, error) {
	return structpb.NewStruct(map[string]any{"errors": []any(validate(r.AsMap())), "warnings": []any{}})
}

func (s *Server) GetCapabilities(_ context.Context, r *structpb.Struct) (*structpb.Struct, error) {
	d := r.AsMap()
	scenario, _ := d["scenario"].(map[string]any)
	errs := validate(scenario)
	for _, field := range []struct{ key, value string }{{"kind", "environment"}, {"version_id", versionID}, {"modality", "text"}} {
		if d[field.key] != field.value {
			errs.add(field.key, "unsupported", "Expected "+field.value)
		}
	}
	return structpb.NewStruct(map[string]any{"supported": len(errs) == 0, "errors": []any(errs), "schema_hashes": map[string]any{"finalization": digest(contracts.FinalizationSchema)}})
}
