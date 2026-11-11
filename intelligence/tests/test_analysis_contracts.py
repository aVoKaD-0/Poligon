from copy import deepcopy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import jsonschema

from poligon_intelligence.analysis import (FindingLabel, HumanScore, KnownDefect, Measurements, Repeat,
    cluster, findings, freeze, metrics, verify_frozen)
from poligon_intelligence.demo import demo_materials
from poligon_intelligence.evaluation import evaluate
from poligon_intelligence.models import Action, Assessment, Scenario, SessionMaterials, dumps
from poligon_intelligence.scenarios import catalog, vary

ROOT = Path(__file__).resolve().parents[1]


class ContractTests(unittest.TestCase):
    def test_upstream_assessment_schema_and_resolvable_evidence(self):
        schema = json.loads((ROOT / "contracts/upstream/platform/contracts/schemas/assessment.json").read_text())
        for case in ("buggy", "fixed", "missing-state", "invalid-simulation", "infrastructure"):
            m = demo_materials(case)
            report = evaluate(m).model_dump()
            jsonschema.validate(report, schema, format_checker=jsonschema.FormatChecker())
            artifacts = {a.artifact_id: json.loads(a.content) for a in (m.snapshot, m.journal, m.audit) if a}
            for assertion in report["assertions"]:
                for ref in assertion["evidence"]:
                    value = artifacts[ref["artifact_id"]]
                    for segment in ref["json_pointer"].split("/")[1:]:
                        key = segment.replace("~1", "/").replace("~0", "~")
                        value = value[int(key)] if isinstance(value, list) else value[key]

    def test_generated_schemas_match_sources(self):
        for model in (Scenario, SessionMaterials, Assessment, Action):
            schema = json.loads((ROOT / f"contracts/schemas/{model.__name__}.json").read_text(encoding="utf-8"))
            self.assertEqual(schema, model.model_json_schema())
        jsonschema.validate(demo_materials().model_dump(), SessionMaterials.model_json_schema())

    def test_pinned_upstream_bytes(self):
        lock = json.loads((ROOT / "contracts/upstream.lock.json").read_text())
        for item in lock:
            self.assertEqual(hashlib.sha256((ROOT / "contracts/upstream" / item["path"]).read_bytes()).hexdigest(), item["sha256"])

    def test_catalog_fixture_scenarios(self):
        for scenario in catalog().values():
            stored = Scenario.model_validate(json.loads((ROOT / "fixtures/scenarios" / f"{scenario.id}.json").read_text(encoding="utf-8")))
            self.assertEqual(stored, scenario)


class AnalysisTests(unittest.TestCase):
    def setUp(self):
        m = demo_materials()
        self.findings = findings("case-1", m.scenario.family, evaluate(m))

    def test_clustering_preserves_every_finding_and_evidence(self):
        result = cluster(self.findings)
        self.assertEqual(sum(len(c["findings"]) for c in result), len(self.findings))
        self.assertTrue(all(f["evidence"] for c in result for f in c["findings"]))
        self.assertEqual(findings("bad-sim", "retry", evaluate(demo_materials("invalid-simulation"))), [])

    def test_known_recall_and_independent_precision(self):
        first = self.findings[0]
        data = Measurements(findings=self.findings,
            known_defects=[KnownDefect(defect_id="injected-1", case_id="case-1", family=first.family, rule_id=first.rule_id),
                          KnownDefect(defect_id="injected-2", case_id="case-2", family=first.family, rule_id=first.rule_id)],
            finding_labels=[FindingLabel(finding_id=first.finding_id, is_defect=True, reviewer_id="reviewer-1", independent=True)])
        result = metrics(data)
        self.assertEqual(result["recall"]["value"], 0.5)
        self.assertEqual(result["precision"]["value"], 1)
        self.assertEqual(result["precision"]["independently_labeled"], 1)

    def test_empty_or_demo_labels_do_not_invent_measurements(self):
        data = Measurements(findings=self.findings, finding_labels=[FindingLabel(
            finding_id=self.findings[0].finding_id, is_defect=True, reviewer_id="DEMO", independent=True, demonstration=True)])
        result = metrics(data)
        self.assertIsNone(result["recall"]["value"])
        self.assertIsNone(result["precision"]["value"])
        self.assertEqual(result["cost"]["status"], "unknown")

    def test_agreement_variance_duration_and_partial_cost(self):
        data = Measurements(human_scores=[HumanScore(case_id="x", criterion="clarity", score=4,
            reviewer_id="r", independent=True)], repeats=[
                Repeat(case_id="x", repeat_id="1", verdict="pass", model_scores={"clarity": 4}, duration_ms=10, cost_minor=2, currency="TEST"),
                Repeat(case_id="x", repeat_id="2", verdict="fail", model_scores={"clarity": 2}, duration_ms=30),
                Repeat(case_id="x", repeat_id="3", verdict="inconclusive")])
        result = metrics(data)
        self.assertEqual(result["agreement"]["clarity"]["exact"], 0.5)
        self.assertEqual(result["agreement"]["clarity"]["mae"], 1)
        self.assertEqual(result["variability"][0]["sample_variance"], 0.5)
        self.assertEqual(result["variability"][0]["excluded"], 1)
        self.assertEqual(result["duration"]["mean_ms"], 20)
        self.assertEqual(result["cost"]["status"], "partial")

    def test_duplicate_labels_rejected(self):
        label = FindingLabel(finding_id=self.findings[0].finding_id, is_defect=True, reviewer_id="r", independent=True)
        with self.assertRaises(ValueError): metrics(Measurements(findings=self.findings, finding_labels=[label, label]))

    def test_freeze_disjoint_families_changes_and_overwrite(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as tmp:
            path = Path(tmp) / "fixed.json"
            scenarios = list(catalog().values()) + [vary(catalog()["repeat"], 1)]
            envelope = freeze(scenarios, path)
            self.assertTrue(verify_frozen(envelope, scenarios))
            with self.assertRaises(FileExistsError): freeze(scenarios, path)
            changed = deepcopy(scenarios)
            changed[0].goal = "changed goal"
            with self.assertRaises(ValueError): verify_frozen(envelope, changed)
            corrupted = deepcopy(envelope); corrupted["manifest"]["items"][0]["split"] = "other"
            with self.assertRaises(ValueError): verify_frozen(corrupted, scenarios)


if __name__ == "__main__": unittest.main()
