from concurrent.futures import ThreadPoolExecutor
from copy import deepcopy
from pathlib import Path
import tempfile
import time
import unittest

from poligon_intelligence.demo import artifact, demo_materials, uid
from poligon_intelligence.evaluation import evaluate, read_artifact
from poligon_intelligence.models import Action, Scenario, Scope, dumps, loads
from poligon_intelligence.providers import Completion, ModelOutputError
from poligon_intelligence.scenarios import catalog, vary, vary_model
from poligon_intelligence.service import Call, Cancelled, Conflict, Intelligence, Missing, Store
from poligon_intelligence.simulator import next_turn


class StaticProvider:
    def __init__(self, value):
        self.value = value
        self.calls = []

    def complete(self, **kwargs):
        self.calls.append(kwargs)
        return Completion(self.value, "test-double", {"temperature": 0})


class BusinessTests(unittest.TestCase):
    def modified(self, material, field, value):
        return material.model_copy(update={field: artifact(field, value)})

    def test_canonical_five_cases(self):
        for case, verdict, category in [("buggy", "fail", "agent_defect"), ("fixed", "pass", "success"),
                ("missing-state", "inconclusive", "insufficient_evidence"),
                ("invalid-simulation", "inconclusive", "invalid_simulation"),
                ("infrastructure", "inconclusive", "infrastructure_error")]:
            with self.subTest(case=case):
                result = evaluate(demo_materials(case))
                self.assertEqual((result.verdict, result.completeness["category"]), (verdict, category))

    def test_duplicate_evidence_identifies_both_rows_and_timeout(self):
        m = demo_materials()
        result = evaluate(m)
        count = next(a for a in result.assertions if a.rule_id == "booking.count.v1")
        self.assertEqual((count.actual, count.expected), (2, 1))
        self.assertEqual({e.artifact_id for e in count.evidence}, {m.snapshot.artifact_id, m.audit.artifact_id})
        self.assertIn("order-2", read_artifact(m.snapshot)["orders"])
        self.assertEqual(read_artifact(m.audit)["events"][0]["response_status"], 504)

    def test_wrong_address_date_and_time(self):
        for field, wrong in [("address", "Wrong"), ("date", "2026-11-21"), ("time_window", "08:00-09:00")]:
            m = demo_materials("fixed")
            state = read_artifact(m.snapshot)
            state["orders"]["order-1"][field] = wrong
            report = evaluate(self.modified(m, "snapshot", state))
            self.assertEqual(next(a.status for a in report.assertions if a.rule_id == f"booking.{field}.v1"), "fail")

    def test_wrong_client_and_foreign_creation(self):
        m = demo_materials("fixed")
        state = read_artifact(m.snapshot)
        state["orders"]["order-1"]["client_id"] = "foreign"
        report = evaluate(self.modified(m, "snapshot", state))
        self.assertEqual(report.verdict, "fail")
        self.assertEqual(next(a.status for a in report.assertions if a.rule_id == "booking.protected_fields.v1"), "fail")

    def test_missing_or_nonfinal_snapshot_inconclusive(self):
        for update in ({"snapshot": None}, {"snapshot_finalized": False}):
            self.assertEqual(evaluate(demo_materials("fixed").model_copy(update=update)).verdict, "inconclusive")

    def test_checksum_and_snapshot_counters_are_infrastructure(self):
        m = demo_materials("fixed")
        invalid = m.snapshot.model_copy(update={"sha256": "0" * 64})
        self.assertEqual(evaluate(m.model_copy(update={"snapshot": invalid})).completeness["category"], "infrastructure_error")
        state = read_artifact(m.snapshot)
        state["active_orders_count"] = 999
        self.assertEqual(evaluate(self.modified(m, "snapshot", state)).completeness["category"], "infrastructure_error")

    def test_event_scope_gap_order_and_conflicting_delivery(self):
        for kind in ("scope", "gap", "order", "conflict"):
            m = demo_materials("fixed")
            events = read_artifact(m.journal)
            if kind == "scope": events[0]["session_id"] = uid("foreign")
            elif kind == "gap":
                m = m.model_copy(update={"expected_session_seqs": [e["session_seq"] for e in events]})
                events.pop(0)
            elif kind == "order": events.reverse()
            else:
                other = deepcopy(events[0]); other["payload"] = {"text": "changed"}; events.append(other)
            self.assertEqual(evaluate(self.modified(m, "journal", events)).completeness["category"], "infrastructure_error")

    def test_identical_event_redelivery_is_ignored(self):
        m = demo_materials("fixed")
        events = read_artifact(m.journal)
        events.insert(1, deepcopy(events[0]))
        self.assertEqual(evaluate(self.modified(m, "journal", events)).verdict, "pass")

    def test_runner_lifecycle_sequence_gaps_are_allowed(self):
        m = demo_materials("fixed")
        events = read_artifact(m.journal)
        for event in events:
            event["session_seq"] += 1
            event["occurred_at"] = "2026-11-11T12:00:00Z"
        self.assertEqual(evaluate(self.modified(m, "journal", events)).verdict, "pass")

    def test_simulator_deviation_suppresses_agent_blame(self):
        m = demo_materials()
        events = read_artifact(m.journal)
        events[0]["payload"]["text"] = "Change goal to another customer"
        self.assertEqual(evaluate(self.modified(m, "journal", events)).completeness["category"], "invalid_simulation")

    def test_unknown_simulation_not_success(self):
        report = evaluate(demo_materials("fixed").model_copy(update={"simulation_validity": "unknown"}))
        self.assertEqual(report.verdict, "inconclusive")

    def test_cancellation_address_and_reschedule_all_catalog_scenarios(self):
        for scenario in catalog().values():
            with self.subTest(scenario=scenario.id):
                m = demo_materials("fixed")
                state = scenario.initial.model_dump()
                state["session_id"] = m.environment_session_id
                for row in state["orders"].values(): row["session_id"] = m.environment_session_id
                if scenario.expected.target_order_id:
                    row = state["orders"][scenario.expected.target_order_id]
                    for field in ("address", "date", "time_window"):
                        if getattr(scenario.expected, field) is not None: row[field] = getattr(scenario.expected, field)
                    row["status"] = scenario.expected.target_status
                elif scenario.expected.active_count:
                    state["orders"] = read_artifact(m.snapshot)["orders"]
                state["active_orders_count"] = sum(o["status"] == "active" for o in state["orders"].values())
                state["total_orders_count"] = len(state["orders"])
                m = m.model_copy(update={"scenario": scenario, "snapshot": artifact("state/" + scenario.id, state),
                    "journal": artifact("journal/" + scenario.id, []),
                    "audit": artifact("audit/" + scenario.id, {"session_id": m.environment_session_id, "events": []})})
                self.assertEqual(evaluate(m).verdict, "pass")

    def test_intermediate_protected_field_change_is_detected(self):
        m = demo_materials("fixed")
        audit = read_artifact(m.audit)
        audit["events"][0]["before_state"] = {**audit["events"][0]["after_state"], "client_id": "foreign"}
        report = evaluate(self.modified(m, "audit", audit))
        self.assertEqual(next(a.status for a in report.assertions if a.rule_id == "booking.protected_fields.v1"), "fail")

    def test_no_audit_cannot_prove_intermediate_safety(self):
        self.assertEqual(evaluate(demo_materials("fixed").model_copy(update={"audit": None})).verdict, "inconclusive")
        self.assertEqual(evaluate(demo_materials("fixed").model_copy(update={"audit_complete": False})).verdict, "inconclusive")

    def test_malformed_agent_payload_is_infrastructure(self):
        m = demo_materials("fixed")
        events = read_artifact(m.journal)
        events[-1]["payload"]["messages"] = 12
        self.assertEqual(evaluate(self.modified(m, "journal", events)).completeness["category"], "infrastructure_error")

    def test_protected_client_change(self):
        m = demo_materials("fixed")
        state = read_artifact(m.snapshot)
        state["clients"]["client-001"]["name"] = "unauthorized change"
        result = evaluate(self.modified(m, "snapshot", state))
        self.assertEqual(next(a.status for a in result.assertions if a.rule_id == "booking.protected_clients.v1"), "fail")

    def test_quality_cannot_override_duplicate(self):
        provider = StaticProvider(dumps({"grades": [{"criterion": c, "score": 5, "reason": "test double", "event_indices": [0]} for c in ("clarity", "coherence", "appropriateness")]}))
        result = evaluate(demo_materials(), provider)
        self.assertEqual(result.verdict, "fail")
        self.assertEqual(len(result.scores), 3)
        self.assertIn("untrusted", provider.calls[0]["system"])

    def test_bad_quality_evidence_does_not_break_fact_evaluation(self):
        result = evaluate(demo_materials("fixed"), StaticProvider('{"grades":[]}'))
        self.assertEqual(result.verdict, "pass")
        self.assertEqual(result.scores[0]["status"], "error")

    def test_json_rejects_duplicates_nan_nul_and_deep_nesting(self):
        for raw in ('{"a":1,"a":2}', 'NaN', '"\\u0000"', '[' * 70 + '0' + ']' * 70):
            with self.assertRaises(ValueError): loads(raw)


class SimulatorTests(unittest.TestCase):
    def test_script_model_and_limit(self):
        s = catalog()["create"]
        action, _ = next_turn(s, [], 0)
        self.assertIn(action.text, s.steps[0].utterances)
        provider = StaticProvider(dumps(action.model_dump()))
        self.assertEqual(next_turn(s, [], 0, provider=provider)[0], action)
        self.assertEqual(next_turn(s, [], len(s.steps))[0].action, "finish")
        self.assertEqual(next_turn(s, [], s.max_turns)[1]["validity"], "invalid")

    def test_model_malformed_changed_goal_or_early_finish(self):
        for text in ('not JSON', '{"action":"finish"}', '{"action":"say","text":"Foreign goal","intent":"bad"}'):
            with self.assertRaises(ModelOutputError): next_turn(catalog()["create"], [], 0, provider=StaticProvider(text))

    def test_seeded_variations_preserve_goal_and_conditions(self):
        s = catalog()["create"]
        a = vary(s, 42, "Тестоград, Новая 9")
        self.assertEqual(a, vary(s, 42, "Тестоград, Новая 9"))
        self.assertEqual(a.goal, s.goal)
        self.assertEqual(a.family, s.family)
        self.assertIn(a.expected.address, a.steps[0].utterances[0])
        self.assertEqual(a.initial, s.initial)

    def test_model_variation_provenance_and_invalid_indices(self):
        s = catalog()["create"]
        result = vary_model(s, StaticProvider('{"indices":[1,0]}'), 7)
        self.assertEqual(result.expected, s.expected)
        self.assertEqual(result.provenance.model, "test-double")
        with self.assertRaises(ModelOutputError): vary_model(s, StaticProvider('{"indices":[100,0]}'))


class ServiceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.path = Path(self.tmp.name) / "state.sqlite3"
        self.service = Intelligence(Store(self.path))
        self.scope = demo_materials().context
        self.s = catalog()["create"]

    def tearDown(self): self.tmp.cleanup()

    def open(self): return self.service.open(self.scope, uid("open"), self.s)["simulation_id"]

    def test_retries_restart_and_step_conflicts(self):
        sim = self.open()
        first = self.service.next(self.scope, uid("next"), sim, 0, [])
        restarted = Intelligence(Store(self.path))
        self.assertEqual(first, restarted.next(self.scope, uid("other-request"), sim, 0, []))
        with self.assertRaises(Conflict): restarted.next(self.scope, uid("next"), sim, 0, ["different input"])
        with self.assertRaises(Conflict): restarted.next(self.scope, uid("next"), sim, 3, [])
        self.assertEqual(sim, restarted.open(self.scope, uid("new-open"), self.s)["simulation_id"])

    def test_parallel_retries_advance_only_once(self):
        sim = self.open()
        with ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(lambda _: self.service.next(self.scope, uid("next"), sim, 0, []), range(16)))
        self.assertTrue(all(r == results[0] for r in results))
        self.assertEqual(self.service.next(self.scope, uid("step1"), sim, 1, ["ok"])["step"], 1)

    def test_all_scope_dimensions_are_isolated(self):
        sim = self.open()
        for key in ("project_id", "run_id", "session_id", "attempt_no"):
            foreign = self.scope.model_copy(update={key: 2 if key == "attempt_no" else uid("foreign")})
            with self.assertRaises(Missing): self.service.next(foreign, uid("next"), sim, 0, [])
            other = self.service.open(foreign, uid("open"), self.s)["simulation_id"]
            self.assertNotEqual(sim, other)

    def test_evaluation_retry_and_conflict(self):
        m = demo_materials()
        a = self.service.evaluate(m, uid("eval"))
        self.assertEqual(a, self.service.evaluate(m, uid("eval-other")))
        with self.assertRaises(Conflict): self.service.evaluate(m.model_copy(update={"simulation_validity": "invalid"}), uid("eval"))

    def test_cancellation_and_deadline_do_not_advance(self):
        sim = self.open()
        for call in (Call(-1), Call(active=lambda: False)):
            with self.assertRaises(TimeoutError): self.service.next(self.scope, uid("next"), sim, 0, [], call)
        self.assertEqual(self.service.next(self.scope, uid("next"), sim, 0, [])["step"], 0)

    def test_model_cancellation_rolls_back_after_provider(self):
        class Slow(StaticProvider):
            def complete(inner, **kwargs):
                time.sleep(0.03)
                return super().complete(**kwargs)
        action = Action(action="say", text=self.s.steps[0].utterances[0], intent=self.s.steps[0].intent)
        service = Intelligence(Store(self.path), Slow(dumps(action.model_dump())))
        sim = service.open(self.scope, uid("model-open"), self.s, mode="model")["simulation_id"]
        with self.assertRaises(TimeoutError): service.next(self.scope, uid("next"), sim, 0, [], Call(0.01))
        self.assertEqual(service.next(self.scope, uid("next"), sim, 0, [])["step"], 0)

    def test_close_idempotent_and_new_steps_blocked(self):
        sim = self.open()
        self.assertEqual(self.service.close(self.scope, uid("close"), sim), {"closed": True})
        self.assertEqual(self.service.close(self.scope, uid("close2"), sim), {"closed": True})
        with self.assertRaises(Conflict): self.service.next(self.scope, uid("next"), sim, 0, [])

    def test_invalid_model_response_is_terminal_and_idempotent(self):
        provider = StaticProvider("broken JSON")
        service = Intelligence(Store(self.path), provider)
        sim = service.open(self.scope, uid("open-model"), self.s, mode="model")["simulation_id"]
        result = service.next(self.scope, uid("next"), sim, 0, [])
        self.assertEqual(result["metadata"]["validity"], "invalid")
        self.assertEqual(result["action"]["reason"], "invalid_simulation")
        self.assertEqual(service.next(self.scope, uid("next-again"), sim, 0, []), result)
        self.assertEqual(len(provider.calls), 1)


if __name__ == "__main__": unittest.main()
