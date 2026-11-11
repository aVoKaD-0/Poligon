"""Run after starting `python -m poligon_intelligence.cli serve --insecure-local`."""
import os
from uuid import uuid4
import grpc
from poligon.control.v1.control_pb2 import ExecutionContext
from poligon.intelligence.v1 import intelligence_pb2 as pb, intelligence_pb2_grpc as rpc
from poligon_intelligence.demo import demo_materials
from poligon_intelligence.models import dumps, loads


def main():
    material = demo_materials("fixed")
    context = ExecutionContext(**material.context.model_dump())
    token = os.environ.get("INTELLIGENCE_TOKEN")
    metadata = [("authorization", "Bearer " + token)] if token else []
    with grpc.insecure_channel("127.0.0.1:50053") as channel:
        client = rpc.IntelligenceServiceStub(channel)
        opened = client.OpenSimulation(pb.OpenSimulationRequest(context=context, request_id=str(uuid4()),
            scenario_json=dumps(material.scenario.model_dump()).encode(), seed=42, mode="scripted"), timeout=5, metadata=metadata)
        request = pb.NextClientActionRequest(context=context, request_id=str(uuid4()), simulation_id=opened.simulation_id, step=0)
        first = client.NextClientAction(request, timeout=5, metadata=metadata)
        replay = client.NextClientAction(request, timeout=5, metadata=metadata)
        assert first == replay
        print("Client:", first.text, "; repeated call returned identical response")
        result = client.EvaluateSession(pb.EvaluateSessionRequest(context=context, request_id=str(uuid4()),
            materials_json=dumps(material.model_dump()).encode()), timeout=5, metadata=metadata)
        assessment = loads(result.assessment_json)
        print("Assessment:", assessment["verdict"], assessment["completeness"]["category"])
        client.CloseSimulation(pb.CloseSimulationRequest(context=context, request_id=str(uuid4()),
            simulation_id=opened.simulation_id), timeout=5, metadata=metadata)


if __name__ == "__main__":
    main()
