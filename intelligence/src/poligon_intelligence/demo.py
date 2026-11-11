"""Explicit TEST DOUBLES for Environment and agent. Not a platform end-to-end run."""
from copy import deepcopy
import hashlib
from pathlib import Path
from uuid import NAMESPACE_URL, uuid5

from .evaluation import evaluate
from .models import Artifact, Scope, SessionMaterials, dumps
from .scenarios import catalog


def uid(name):
    return str(uuid5(NAMESPACE_URL, "poligon-intelligence-demo/" + name))


def artifact(name, value):
    content = dumps(value)
    return Artifact(artifact_id=uid(name), sha256=hashlib.sha256(content.encode()).hexdigest(), content=content)


class FixtureEnvironment:
    """Only simulates create/read plus one timeout; no production service replacement."""
    def __init__(self, scope, scenario):
        self.scope, self.scenario = scope, scenario
        self.state = deepcopy(scenario.initial.model_dump())
        self.state["session_id"] = "fixture-attempt/" + scope.key()
        self.audit = []

    def create(self):
        index = len(self.state["orders"]) + 1
        order_id = f"order-{index}"
        order = dict(id=order_id, session_id=self.state["session_id"], client_id=self.scenario.client_id,
                     address=self.scenario.expected.address, date=self.scenario.expected.date,
                     time_window=self.scenario.expected.time_window, status="active")
        self.state["orders"][order_id] = order
        self.state["active_orders_count"] = self.state["total_orders_count"] = index
        status = 504 if index == 1 else 201
        self.audit.append(dict(id=uid(self.scope.key() + f"/audit/{index}"), session_id=self.state["session_id"],
            action="CREATE_ORDER", entity_type="order", entity_id=order_id, method="POST", path="/api/v1/orders",
            response_status=status, after_state=deepcopy(order), fault_injected="timeout_after_save" if status == 504 else ""))
        return status, {"error": "timeout"} if status == 504 else deepcopy(order)


def demo_materials(case="buggy"):
    scenario = catalog()["timeout-after-save"]
    scope = Scope(project_id=uid("project"), run_id=uid("run"), session_id=uid(case), attempt_no=1)
    env = FixtureEnvironment(scope, scenario)
    journal = []
    def event(kind, payload):
        seq = len(journal) + 1
        journal.append(dict(**scope.model_dump(), schema_version=1, producer_event_id=uid(case + f"/event/{seq}"),
                            session_seq=seq, event_kind=kind, payload=payload))
    event("client.message", {"text": scenario.steps[0].utterances[0], "turn": 1})
    event("tool.request", {"id": "call-1", "name": "create_order", "arguments": {"client_id": scenario.client_id}})
    status, body = env.create()
    event("tool.result", {"result": {"id": "call-1", "http_status": status, "body": body}})
    if case == "fixed":
        event("tool.request", {"id": "read-1", "name": "list_orders", "arguments": {"client_id": scenario.client_id}})
        event("tool.result", {"result": {"id": "read-1", "http_status": 200, "body": list(env.state["orders"].values())}})
    else:
        event("tool.request", {"id": "call-2", "name": "create_order", "arguments": {"client_id": scenario.client_id}})
        status, body = env.create()
        event("tool.result", {"result": {"id": "call-2", "http_status": status, "body": body}})
    event("agent.message", {"messages": ["Заявка создана."], "finished": True})
    return SessionMaterials(context=scope, scenario=scenario, evaluation_id=uid(case + "/evaluation"),
        evaluator_version_id=uid("evaluator.v1"), manifest_sha256=hashlib.sha256(b"fixture manifest, not registered with Platform").hexdigest(),
        environment_session_id=env.state["session_id"], snapshot=artifact(case + "/snapshot", env.state) if case != "missing-state" else None,
        audit=artifact(case + "/audit", {"session_id": env.state["session_id"], "events": env.audit}),
        journal=artifact(case + "/journal", journal), snapshot_finalized=case != "missing-state", journal_complete=True, audit_complete=True,
        simulation_validity="invalid" if case == "invalid-simulation" else "valid",
        infrastructure_error="Fixture infrastructure failure" if case == "infrastructure" else None)


def run_demo(output: Path):
    output.mkdir(parents=True, exist_ok=True)
    summaries = []
    for case in ("buggy", "fixed", "missing-state", "invalid-simulation", "infrastructure"):
        materials = demo_materials(case)
        assessment = evaluate(materials)
        path = output / case
        path.mkdir(exist_ok=True)
        (path / "materials.json").write_text(dumps(materials.model_dump()), encoding="utf-8")
        (path / "assessment.json").write_text(dumps(assessment.model_dump()), encoding="utf-8")
        artifacts = path / "artifacts"
        artifacts.mkdir(exist_ok=True)
        for item in (materials.snapshot, materials.journal, materials.audit):
            if item:
                (artifacts / f"{item.artifact_id}.json").write_text(item.content, encoding="utf-8")
        summaries.append({"case": case, "verdict": assessment.verdict, "category": assessment.completeness["category"]})
    (output / "summary.json").write_text(dumps({"test_doubles": True, "cases": summaries}), encoding="utf-8")
    return summaries
