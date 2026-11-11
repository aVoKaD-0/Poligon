"""Explainable exact-rule clustering, independent labels, and family-level freezing."""
from collections import Counter, defaultdict
from datetime import datetime, timezone
import hashlib
from pathlib import Path
import statistics
from typing import Literal

from pydantic import Field
from .models import Assessment, Evidence, Model, Scenario, dumps, loads


class Finding(Model):
    finding_id: str
    case_id: str
    family: str
    rule_id: str
    expected: object
    actual: object
    reason: str
    evidence: list[Evidence]


def findings(case_id: str, family: str, report: Assessment) -> list[Finding]:
    if report.completeness.get("category") != "agent_defect":
        return []
    return [Finding(finding_id=hashlib.sha256(f"{case_id}/{a.rule_id}".encode()).hexdigest()[:24],
                    case_id=case_id, family=family, rule_id=a.rule_id, expected=a.expected,
                    actual=a.actual, reason=a.reason, evidence=a.evidence)
            for a in report.assertions if a.required and a.status == "fail"]


def cluster(items: list[Finding]) -> list[dict]:
    groups = defaultdict(list)
    for item in items:
        groups[(item.family, item.rule_id)].append(item)
    return [{"cluster_id": hashlib.sha256(dumps(key).encode()).hexdigest()[:16],
             "family": key[0], "rule_id": key[1], "method": "same family and versioned failing rule",
             "findings": [item.model_dump() for item in values]}
            for key, values in sorted(groups.items())]


class KnownDefect(Model):
    defect_id: str
    case_id: str
    family: str
    rule_id: str


class FindingLabel(Model):
    finding_id: str
    is_defect: bool
    reviewer_id: str
    independent: bool
    demonstration: bool = False


class HumanScore(Model):
    case_id: str
    criterion: Literal["clarity", "coherence", "appropriateness"]
    score: int = Field(ge=1, le=5)
    reviewer_id: str
    independent: bool
    demonstration: bool = False


class Repeat(Model):
    case_id: str
    repeat_id: str
    verdict: Literal["pass", "fail", "inconclusive"]
    model_scores: dict[str, int] = Field(default_factory=dict)
    duration_ms: float | None = Field(default=None, ge=0, allow_inf_nan=False)
    cost_minor: int | None = Field(default=None, ge=0)
    currency: str | None = None


class Measurements(Model):
    known_defects: list[KnownDefect] = Field(default_factory=list)
    findings: list[Finding] = Field(default_factory=list)
    finding_labels: list[FindingLabel] = Field(default_factory=list)
    human_scores: list[HumanScore] = Field(default_factory=list)
    repeats: list[Repeat] = Field(default_factory=list)


def unique(values, key):
    ids = [key(v) for v in values]
    if len(set(ids)) != len(ids):
        raise ValueError("duplicate measurement identity")


def metrics(data: Measurements) -> dict:
    unique(data.findings, lambda v: v.finding_id)
    unique(data.known_defects, lambda v: v.defect_id)
    unique(data.known_defects, lambda v: (v.case_id, v.family, v.rule_id))
    unique(data.repeats, lambda v: (v.case_id, v.repeat_id))
    labels = [x for x in data.finding_labels if x.independent and not x.demonstration]
    # One adjudicated label per finding. Multiple reviews require external adjudication.
    unique(labels, lambda v: v.finding_id)
    by_id = {x.finding_id: x for x in data.findings}
    if any(label.finding_id not in by_id for label in labels):
        raise ValueError("human finding label references unknown finding")
    detected = {(x.case_id, x.family, x.rule_id) for x in data.findings}
    recalled = sum((x.case_id, x.family, x.rule_id) in detected for x in data.known_defects)
    known = len(data.known_defects)
    result = {
        "recall": {"value": recalled / known if known else None, "detected": recalled, "known": known},
        "precision": {"value": sum(x.is_defect for x in labels) / len(labels) if labels else None,
                      "true_positive": sum(x.is_defect for x in labels), "independently_labeled": len(labels),
                      "unlabeled": len(data.findings) - len(labels)},
        "agreement": {}, "variability": [],
    }
    human = [x for x in data.human_scores if x.independent and not x.demonstration]
    unique(human, lambda v: (v.case_id, v.criterion))
    for criterion in ("clarity", "coherence", "appropriateness"):
        pairs = [(r.model_scores[criterion], label.score) for label in human if label.criterion == criterion
                 for r in data.repeats if r.case_id == label.case_id and criterion in r.model_scores]
        if any(not 1 <= score <= 5 for r in data.repeats for score in r.model_scores.values()):
            raise ValueError("model scores must be 1..5")
        n = len(pairs)
        observed = sum((a - b)**2 / 16 for a, b in pairs) / n if n else None
        aa, bb = Counter(a for a, _ in pairs), Counter(b for _, b in pairs)
        expected = sum(aa[a] * bb[b] * (a - b)**2 / 16 for a in aa for b in bb) / n**2 if n else None
        result["agreement"][criterion] = {"n": n,
            "exact": sum(a == b for a, b in pairs) / n if n else None,
            "mae": sum(abs(a - b) for a, b in pairs) / n if n else None,
            "quadratic_weighted_kappa": 1 - observed / expected if expected else None}
    groups = defaultdict(list)
    for r in data.repeats:
        groups[r.case_id].append(r)
    for case, values in sorted(groups.items()):
        binary = [int(v.verdict == "pass") for v in values if v.verdict in {"pass", "fail"}]
        result["variability"].append({"case_id": case, "eligible_repeats": len(binary),
            "excluded": len(values) - len(binary), "pass_rate": statistics.mean(binary) if binary else None,
            "sample_variance": statistics.variance(binary) if len(binary) > 1 else None,
            "scores": {criterion: {"n": len(scores), "sample_stdev": statistics.stdev(scores) if len(scores) > 1 else None}
                       for criterion in ("clarity", "coherence", "appropriateness")
                       if (scores := [v.model_scores[criterion] for v in values if criterion in v.model_scores])}})
    durations = [r.duration_ms for r in data.repeats if r.duration_ms is not None]
    costs = defaultdict(list)
    for r in data.repeats:
        if r.cost_minor is not None:
            if not r.currency:
                raise ValueError("known cost requires currency")
            costs[r.currency].append(r.cost_minor)
    result["duration"] = {"observed": len(durations), "mean_ms": statistics.mean(durations) if durations else None}
    result["cost"] = {"status": "unknown" if not costs else "complete" if sum(map(len, costs.values())) == len(data.repeats) else "partial",
                      "known_subtotals_minor": {currency: sum(values) for currency, values in costs.items()},
                      "unknown_repeats": sum(r.cost_minor is None for r in data.repeats)}
    return result


def freeze(scenarios: list[Scenario], path: Path, seed=0):
    unique(scenarios, lambda s: (s.id, s.version))
    families = sorted({s.family for s in scenarios}, key=lambda f: hashlib.sha256(f"{seed}:{f}".encode()).hexdigest())
    if len(families) < 2:
        raise ValueError("at least two independent families needed")
    holdout = set(families[:max(1, len(families) // 3)])
    body = {"schema_version": 1, "frozen_at": datetime.now(timezone.utc).isoformat(), "seed": seed,
            "items": [{"id": s.id, "version": s.version, "family": s.family,
                       "split": "holdout" if s.family in holdout else "tuning",
                       "sha256": hashlib.sha256(dumps(s.model_dump()).encode()).hexdigest()} for s in sorted(scenarios, key=lambda s: s.id)]}
    envelope = {"sha256": hashlib.sha256(dumps(body).encode()).hexdigest(), "manifest": body}
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("x", encoding="utf-8") as f:  # Never silently replace a fixed final set.
        f.write(dumps(envelope))
    return envelope


def verify_frozen(envelope: dict, scenarios: list[Scenario]):
    manifest = envelope["manifest"]
    if hashlib.sha256(dumps(manifest).encode()).hexdigest() != envelope["sha256"]:
        raise ValueError("frozen manifest tampered")
    expected = {(s.id, s.version): s for s in scenarios}
    unique(manifest["items"], lambda item: (item["id"], item["version"]))
    if set(expected) != {(i["id"], i["version"]) for i in manifest["items"]}:
        raise ValueError("scenario membership changed after freezing")
    families = defaultdict(set)
    for item in manifest["items"]:
        scenario = expected[(item["id"], item["version"])]
        if item["family"] != scenario.family or item["split"] not in {"tuning", "holdout"} or hashlib.sha256(dumps(scenario.model_dump()).encode()).hexdigest() != item["sha256"]:
            raise ValueError("scenario changed after freezing")
        families[item["family"]].add(item["split"])
    if any(len(split) != 1 for split in families.values()):
        raise ValueError("family leakage between tuning and holdout")
    return True
