from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
import tempfile
import threading
import unittest
from uuid import uuid4

import grpc
from google.protobuf.json_format import ParseDict, MessageToDict
from google.protobuf.struct_pb2 import Struct
from poligon.control.v1.control_pb2 import ExecutionContext
from poligon.intelligence.v1 import intelligence_pb2 as pb, intelligence_pb2_grpc as rpc
from poligon_intelligence.demo import demo_materials, uid
from poligon_intelligence.grpc_server import build_server
from poligon_intelligence.models import dumps, loads
from poligon_intelligence.service import Intelligence, Store


class GrpcTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.service = Intelligence(Store(Path(self.tmp.name) / "rpc.sqlite3"))
        self.server = build_server(self.service, token="local-test-token")
        self.port = self.server.add_insecure_port("127.0.0.1:0")
        self.server.start()
        self.channel = grpc.insecure_channel(f"127.0.0.1:{self.port}")
        self.client = rpc.IntelligenceServiceStub(self.channel)
        self.m = demo_materials("fixed")
        self.ctx = ExecutionContext(**self.m.context.model_dump())
        self.metadata = [("authorization", "Bearer local-test-token")]

    def tearDown(self):
        self.channel.close()
        self.server.stop(0).wait()
        self.tmp.cleanup()

    def invoke(self, method, request, **kwargs):
        return method(request, timeout=3, metadata=self.metadata, **kwargs)

    def open_request(self):
        return pb.OpenSimulationRequest(context=self.ctx, request_id=str(uuid4()),
            scenario_json=dumps(self.m.scenario.model_dump()).encode(), seed=42, mode="scripted")

    def test_real_rpc_lifecycle_and_retries(self):
        request = self.open_request()
        opened = self.invoke(self.client.OpenSimulation, request)
        self.assertEqual(opened, self.invoke(self.client.OpenSimulation, request))
        next_request = pb.NextClientActionRequest(context=self.ctx, request_id=str(uuid4()), simulation_id=opened.simulation_id, step=0)
        with ThreadPoolExecutor(max_workers=6) as pool:
            replies = list(pool.map(lambda _: self.invoke(self.client.NextClientAction, next_request), range(8)))
        self.assertTrue(all(r == replies[0] for r in replies))
        self.assertEqual(replies[0].action, "say")
        assessment_request = pb.EvaluateSessionRequest(context=self.ctx, request_id=str(uuid4()), materials_json=dumps(self.m.model_dump()).encode())
        report = self.invoke(self.client.EvaluateSession, assessment_request)
        self.assertEqual(loads(report.assessment_json)["verdict"], "pass")
        self.assertEqual(report, self.invoke(self.client.EvaluateSession, assessment_request))
        closed = self.invoke(self.client.CloseSimulation, pb.CloseSimulationRequest(context=self.ctx,
            request_id=str(uuid4()), simulation_id=opened.simulation_id))
        self.assertTrue(closed.closed)

    def test_auth_deadline_and_invalid_json(self):
        with self.assertRaises(grpc.RpcError) as error:
            self.client.OpenSimulation(self.open_request(), timeout=3)
        self.assertEqual(error.exception.code(), grpc.StatusCode.UNAUTHENTICATED)
        with self.assertRaises(grpc.RpcError) as error:
            self.client.OpenSimulation(self.open_request(), metadata=self.metadata)
        self.assertEqual(error.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)
        request = self.open_request(); request.scenario_json = b'{"schema_version":1,"schema_version":1}'
        with self.assertRaises(grpc.RpcError) as error: self.invoke(self.client.OpenSimulation, request)
        self.assertEqual(error.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    def test_scope_conflict_and_other_scope_not_found(self):
        opened = self.invoke(self.client.OpenSimulation, self.open_request())
        self.ctx.attempt_no = 2
        request = pb.NextClientActionRequest(context=self.ctx, request_id=str(uuid4()), simulation_id=opened.simulation_id)
        with self.assertRaises(grpc.RpcError) as error: self.invoke(self.client.NextClientAction, request)
        self.assertEqual(error.exception.code(), grpc.StatusCode.NOT_FOUND)
        request = pb.EvaluateSessionRequest(context=self.ctx, request_id=str(uuid4()), materials_json=dumps(self.m.model_dump()).encode())
        with self.assertRaises(grpc.RpcError) as error: self.invoke(self.client.EvaluateSession, request)
        self.assertEqual(error.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    def platform_definition(self):
        s = self.m.scenario
        return dict(schema_version=1, modality="text", actor_customer_ref=s.client_id, client_goal=s.goal,
            fault_rules=s.fault_rules, initial_fixture={"clients": list(s.initial.clients.values()),
                "slots": list(s.initial.slots.values())}, limits={"max_client_turns": s.max_turns},
            client_known_facts={"intelligence_scenario": s.model_dump()})

    def test_platform_struct_proposal_validation_and_capabilities(self):
        scenario = self.platform_definition()
        result = self.invoke(self.client.ValidateScenario, ParseDict(scenario, Struct()))
        self.assertEqual(MessageToDict(result)["errors"], [])
        cap = dict(modality="text", kind="evaluator", version_id=uid("evaluator.v1"), scenario=scenario)
        result = self.invoke(self.client.GetCapabilities, ParseDict(cap, Struct()))
        self.assertTrue(MessageToDict(result)["supported"])
        cap["modality"] = "voice"
        result = self.invoke(self.client.GetCapabilities, ParseDict(cap, Struct()))
        self.assertFalse(MessageToDict(result)["supported"])
        scenario["fault_rules"] = []
        result = self.invoke(self.client.ValidateScenario, ParseDict(scenario, Struct()))
        self.assertTrue(MessageToDict(result)["errors"])

    def test_rpc_cancellation_rolls_back_inflight_model_step(self):
        from poligon_intelligence.providers import Completion
        started, release = threading.Event(), threading.Event()
        s = self.m.scenario
        class Provider:
            def complete(inner, **kwargs):
                started.set()
                release.wait(2)
                return Completion(dumps({"action": "say", "text": s.steps[0].utterances[0], "intent": s.steps[0].intent}), "slow-test-double", {})
        self.service.provider = Provider()
        request = self.open_request(); request.mode = "model"
        opened = self.invoke(self.client.OpenSimulation, request)
        request = pb.NextClientActionRequest(context=self.ctx, request_id=str(uuid4()), simulation_id=opened.simulation_id, step=0)
        pending = self.client.NextClientAction.future(request, timeout=3, metadata=self.metadata)
        self.assertTrue(started.wait(1))
        self.assertTrue(pending.cancel())
        release.set()
        reply = self.invoke(self.client.NextClientAction, request)
        self.assertEqual(reply.step, 0)
        self.assertEqual(reply.action, "say")
        self.assertEqual(reply, self.invoke(self.client.NextClientAction, request))


if __name__ == "__main__": unittest.main()
