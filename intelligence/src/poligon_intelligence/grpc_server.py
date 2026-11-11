"""Proposed provider-owned RPC adapter. Local-only server, no broker/lease worker."""
from concurrent import futures
import hmac
import os
import hashlib

import grpc
from google.protobuf.json_format import MessageToDict, ParseDict
from google.protobuf.struct_pb2 import Struct

from poligon.intelligence.v1 import intelligence_pb2 as pb, intelligence_pb2_grpc as rpc
from .models import ID, Scenario, Scope, SessionMaterials, dumps, loads
from .providers import ModelOutputError
from .service import Call, Cancelled, Conflict, Intelligence, Missing, Store
from pydantic import TypeAdapter


class Adapter(rpc.IntelligenceServiceServicer):
    def __init__(self, service, token="", insecure_local=False):
        if not token and not insecure_local:
            raise ValueError("set INTELLIGENCE_TOKEN or opt into --insecure-local")
        self.service, self.token = service, token

    def invoke(self, context, operation):
        if self.token:
            tokens = [value for key, value in context.invocation_metadata() if key == "authorization"]
            if len(tokens) != 1 or not hmac.compare_digest(tokens[0], "Bearer " + self.token):
                context.abort(grpc.StatusCode.UNAUTHENTICATED, "service credential required")
        remaining = context.time_remaining()
        if remaining is None or remaining > 30:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "explicit deadline <=30s required")
        call = Call(remaining, active=context.is_active)
        try:
            return operation(call)
        except Conflict:
            context.abort(grpc.StatusCode.ALREADY_EXISTS, "conflicting request or step")
        except Missing:
            context.abort(grpc.StatusCode.NOT_FOUND, "simulation not found in execution scope")
        except Cancelled:
            context.abort(grpc.StatusCode.CANCELLED, "request cancelled")
        except TimeoutError:
            context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, "deadline exceeded")
        except ModelOutputError:
            context.abort(grpc.StatusCode.FAILED_PRECONDITION, "SIMULATION_INVALID: model response violates scenario")
        except (ValueError, TypeError, RecursionError):
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "invalid v1 input")
        except OSError:
            context.abort(grpc.StatusCode.UNAVAILABLE, "dependency unavailable")
        except Exception:
            # Raw inputs/provider errors may contain secrets or hostile transcript strings.
            context.abort(grpc.StatusCode.INTERNAL, "internal Intelligence error")

    def scope(self, request):
        TypeAdapter(ID).validate_python(request.request_id)
        return Scope(project_id=request.context.project_id, run_id=request.context.run_id,
                     session_id=request.context.session_id, attempt_no=request.context.attempt_no)

    def OpenSimulation(self, request, context):
        def operation(call):
            value = self.service.open(self.scope(request), request.request_id,
                Scenario.model_validate(loads(request.scenario_json)), request.seed, request.mode, call)
            return pb.OpenSimulationResponse(**value)
        return self.invoke(context, operation)

    def NextClientAction(self, request, context):
        def operation(call):
            TypeAdapter(ID).validate_python(request.simulation_id)
            value = self.service.next(self.scope(request), request.request_id, request.simulation_id,
                request.step, list(request.agent_messages), call)
            return pb.NextClientActionResponse(simulation_id=value["simulation_id"], step=value["step"],
                **value["action"], metadata_json=dumps(value["metadata"]).encode())
        return self.invoke(context, operation)

    def CloseSimulation(self, request, context):
        def operation(call):
            TypeAdapter(ID).validate_python(request.simulation_id)
            return pb.CloseSimulationResponse(**self.service.close(self.scope(request), request.request_id, request.simulation_id, call))
        return self.invoke(context, operation)

    def EvaluateSession(self, request, context):
        def operation(call):
            scope = self.scope(request)
            materials = SessionMaterials.model_validate(loads(request.materials_json))
            if materials.context != scope:
                raise ValueError("RPC/body scope mismatch")
            result = self.service.evaluate(materials, request.request_id, call, request.quality)
            return pb.EvaluateSessionResponse(assessment_json=dumps(result).encode())
        return self.invoke(context, operation)

    def ValidateScenario(self, request, context):
        def operation(call):
            data = struct_data(request)
            try:
                scenario = platform_scenario(data)
                Scenario.model_validate(scenario)
                errors = []
            except (ValueError, KeyError, TypeError):
                errors = [{"path": "/", "code": "UNSUPPORTED_SCENARIO", "message": "Use an Intelligence MVP definition in client_known_facts.intelligence_scenario"}]
            return ParseDict({"errors": errors, "warnings": []}, Struct())
        return self.invoke(context, operation)

    def GetCapabilities(self, request, context):
        def operation(call):
            from .demo import uid
            data = struct_data(request)
            version = data.get("version_id")
            kind = data.get("kind")
            expected_version = (os.environ.get("INTELLIGENCE_SIMULATOR_VERSION_ID" if kind == "simulator" else "INTELLIGENCE_EVALUATOR_VERSION_ID")
                                or uid("simulator.v1" if kind == "simulator" else "evaluator.v1"))
            supported = (data.get("modality") == "text" and kind in {"simulator", "evaluator"}
                         and version == expected_version)
            try:
                platform_scenario(data["scenario"])
            except (ValueError, KeyError, TypeError):
                supported = False
            return ParseDict({"supported": supported, "errors": [] if supported else [{"path": "/", "code": "UNSUPPORTED", "message": "Unregistered version, modality or scenario"}],
                              "schema_hashes": {"Scenario": hashlib.sha256(dumps(Scenario.model_json_schema()).encode()).hexdigest(),
                                                "SessionMaterials": hashlib.sha256(dumps(SessionMaterials.model_json_schema()).encode()).hexdigest()},
                              "capability_version": "text.v1"}, Struct())
        return self.invoke(context, operation)


def platform_scenario(data):
    """Explicit bridge for the open Struct proposal. Do not claim arbitrary scenarios work."""
    scenario = Scenario.model_validate(data["client_known_facts"]["intelligence_scenario"])
    if scenario.initial.orders:
        raise ValueError("pinned Runner/Environment cannot initialize existing orders")
    if (data.get("schema_version") != 1 or data.get("modality") != "text"
            or data.get("actor_customer_ref") != scenario.client_id or data.get("client_goal") != scenario.goal
            or data.get("fault_rules") != scenario.fault_rules
            or data.get("initial_fixture") != {"clients": list(scenario.initial.clients.values()),
                "slots": list(scenario.initial.slots.values())}
            or data.get("limits", {}).get("max_client_turns") != scenario.max_turns):
        raise ValueError("Platform/scenario mismatch")
    return scenario.model_dump()


def struct_data(request):
    # Struct uses doubles even for integer JSON fields. Convert exact safe integers
    # before strict provider schema validation; reject out-of-range values there.
    def normalize(value):
        if isinstance(value, float) and value.is_integer() and abs(value) <= 2**53 - 1:
            return int(value)
        if isinstance(value, dict):
            return {k: normalize(v) for k, v in value.items()}
        if isinstance(value, list):
            return [normalize(v) for v in value]
        return value
    return normalize(MessageToDict(request))


def build_server(service, token="", insecure_local=False):
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8), options=[
        ("grpc.max_receive_message_length", 16 * 1024 * 1024), ("grpc.max_send_message_length", 16 * 1024 * 1024)])
    rpc.add_IntelligenceServiceServicer_to_server(Adapter(service, token, insecure_local), server)
    return server


def serve(bind="127.0.0.1:50053", db="output/intelligence.sqlite3", insecure_local=False, provider=None):
    if not (bind.startswith("127.0.0.1:") or bind.startswith("[::1]:")):
        raise ValueError("MVP supports local loopback only; deployment TLS/authorization require team integration")
    server = build_server(Intelligence(Store(db), provider), os.environ.get("INTELLIGENCE_TOKEN", ""), insecure_local)
    if not server.add_insecure_port(bind):
        raise OSError("cannot bind Intelligence server")
    server.start()
    print(f"Intelligence proposal listening at {bind}", flush=True)
    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        server.stop(2).wait()
