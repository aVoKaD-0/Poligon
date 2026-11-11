"""Provider-owned v1 data contracts. Upstream assessment remains unchanged."""
from __future__ import annotations

import json
import math
from typing import Annotated, Any, Literal

from pydantic import BaseModel, ConfigDict, Field, StringConstraints, model_validator

ID = Annotated[str, StringConstraints(pattern=r"^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$")]
SHA = Annotated[str, StringConstraints(pattern=r"^[0-9a-f]{64}$")]
Text = Annotated[str, StringConstraints(min_length=1, max_length=24000)]


class Model(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True, validate_assignment=True)


class Scope(Model):
    project_id: ID
    run_id: ID
    session_id: ID
    attempt_no: int = Field(ge=1, le=2**32 - 1)

    def key(self) -> str:
        return "/".join(str(v) for v in self.model_dump().values())


class Order(Model):
    # Environment fields not needed by assertions are retained for protected-field checks.
    model_config = ConfigDict(extra="allow", strict=True)
    id: Text
    session_id: Text
    client_id: Text
    address: Text
    date: str = Field(pattern=r"^\d{4}-\d{2}-\d{2}$")
    time_window: Text
    status: Literal["active", "cancelled", "completed"]


class Snapshot(Model):
    model_config = ConfigDict(extra="allow", strict=True)
    session_id: Text
    clients: dict[str, dict[str, Any]]
    slots: dict[str, dict[str, Any]]
    orders: dict[str, Order]
    active_orders_count: int = Field(ge=0)
    total_orders_count: int = Field(ge=0)

    @model_validator(mode="after")
    def consistency(self):
        if any(k != v.id or v.session_id != self.session_id for k, v in self.orders.items()):
            raise ValueError("snapshot order identity/session mismatch")
        if self.total_orders_count != len(self.orders) or self.active_orders_count != sum(
            o.status == "active" for o in self.orders.values()
        ):
            raise ValueError("snapshot counters disagree with rows")
        return self


class Expected(Model):
    active_count: int = Field(ge=0)
    address: Text | None = None
    date: str | None = None
    time_window: Text | None = None
    target_order_id: str | None = None
    target_status: Literal["active", "cancelled"] | None = None


class Step(Model):
    # A finite catalog makes checking simulator adherence objective in this MVP.
    intent: Text
    utterances: list[Text] = Field(min_length=1, max_length=20)
    requires_question: bool = False


class Provenance(Model):
    origin: Literal["manual", "seeded", "model"] = "manual"
    parent_id: str | None = None
    seed: int | None = Field(default=None, ge=-(2**53 - 1), le=2**53 - 1)
    parameters: dict[str, Any] = Field(default_factory=dict)
    model: str | None = None
    settings: dict[str, Any] = Field(default_factory=dict)
    prompt_version: str | None = None


class Scenario(Model):
    schema_version: Literal[1] = 1
    id: Text
    version: Text
    family: Text
    goal: Text
    client_id: Text
    initial: Snapshot
    expected: Expected
    constraints: list[Text] = Field(min_length=1)
    allowed_outcomes: list[Text] = Field(min_length=1)
    termination: Literal["script_exhausted"] = "script_exhausted"
    protected_order_fields: list[str] = Field(default_factory=lambda: ["client_id"])
    steps: list[Step] = Field(min_length=1, max_length=50)
    max_turns: int = Field(ge=1, le=50)
    # Declarative only: exclusively executed by Environment, never by the simulator.
    fault_rules: list[dict[str, Any]] = Field(default_factory=list)
    provenance: Provenance = Field(default_factory=Provenance)

    @model_validator(mode="after")
    def coherent(self):
        from datetime import date
        if self.client_id not in self.initial.clients:
            raise ValueError("scenario client absent from initial conditions")
        if self.max_turns < len(self.steps):
            raise ValueError("max_turns cannot truncate the required script")
        if self.expected.date:
            date.fromisoformat(self.expected.date)
        if self.expected.target_order_id:
            target = self.initial.orders.get(self.expected.target_order_id)
            if not target or target.client_id != self.client_id:
                raise ValueError("target order absent or belongs to another client")
        if self.provenance.origin == "model" and not (
            self.provenance.model and self.provenance.prompt_version
        ):
            raise ValueError("model generation requires model and prompt provenance")
        return self


class Message(Model):
    role: Literal["agent", "client"]
    text: Text


class Action(Model):
    action: Literal["say", "finish"]
    text: str = Field(default="", max_length=24000)
    intent: str = ""
    reason: str = ""

    @model_validator(mode="after")
    def valid_action(self):
        if "\x00" in self.text or (self.action == "say" and not self.text.strip()):
            raise ValueError("invalid client message")
        if self.action == "finish" and (self.text or self.intent):
            raise ValueError("finish cannot include a message/intent")
        return self


class Evidence(Model):
    artifact_id: ID
    json_pointer: str = Field(pattern=r"^(/.*)?$")


class Event(Scope):
    schema_version: Literal[1] = 1
    producer_event_id: ID
    session_seq: int = Field(ge=1)
    event_kind: Text
    occurred_at: str | None = None
    payload: dict[str, Any]


class Artifact(Model):
    artifact_id: ID
    sha256: SHA
    # Registered bytes are represented by their exact UTF-8 string, not reserialized JSON.
    content: str = Field(max_length=16 * 1024 * 1024)


class SessionMaterials(Model):
    schema_version: Literal[1] = 1
    context: Scope
    evaluation_id: ID
    evaluator_version_id: ID
    manifest_sha256: SHA
    scenario: Scenario
    environment_session_id: Text
    snapshot: Artifact | None = None
    # JSON array of Runner event envelopes, sorted by global session_seq.
    journal: Artifact | None = None
    # JSON object {session_id, events:[Environment AuditEvent]}.
    audit: Artifact | None = None
    audit_complete: bool = False
    snapshot_finalized: bool = False
    journal_complete: bool = False
    # Runner allocates session_seq to live lifecycle events too: gaps in artifact
    # journals are legal. Exact coverage is checked only with an explicit index.
    expected_session_seqs: list[Annotated[int, Field(ge=1)]] | None = None
    simulation_validity: Literal["valid", "invalid", "unknown"] = "unknown"
    infrastructure_error: str | None = None
    duration_ms: float | None = Field(default=None, ge=0, allow_inf_nan=False)
    cost_minor: int | None = Field(default=None, ge=0)
    currency: str | None = None


class Assertion(Model):
    assertion_id: Text
    # Version is embedded in rule_id because upstream forbids a separate field.
    rule_id: Text
    required: bool = True
    status: Literal["pass", "fail", "inconclusive", "error", "not_applicable"]
    expected: Any = None
    actual: Any = None
    reason: str
    applicability: dict[str, Any] = Field(default_factory=dict)
    evidence: list[Evidence] = Field(default_factory=list)


class Assessment(Scope):
    schema_version: Literal[1] = 1
    evaluation_id: ID
    evaluator_version_id: ID
    manifest_sha256: SHA
    verdict: Literal["pass", "fail", "inconclusive"]
    simulation_validity: Literal["valid", "invalid", "unknown"]
    assertions: list[Assertion]
    scores: list[dict[str, Any]] = Field(default_factory=list)
    completeness: dict[str, Any]
    usage: dict[str, Any]


def loads(raw: str | bytes) -> Any:
    """Reject duplicate keys, NaN, NUL and excessive nesting at every trust boundary."""
    if len(raw) > 16 * 1024 * 1024:
        raise ValueError("JSON exceeds 16 MiB")
    def pairs(items):
        result = {}
        for k, v in items:
            if k in result:
                raise ValueError("duplicate JSON key")
            result[k] = v
        return result
    result = json.loads(raw, object_pairs_hook=pairs, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))
    def check(v, depth=0):
        if depth > 64:
            raise ValueError("JSON nesting limit")
        if isinstance(v, str) and "\x00" in v:
            raise ValueError("NUL in JSON")
        if isinstance(v, float) and not math.isfinite(v):
            raise ValueError("nonfinite JSON")
        if isinstance(v, dict):
            for k, x in v.items():
                check(k, depth + 1)
                check(x, depth + 1)
        if isinstance(v, list):
            for x in v:
                check(x, depth + 1)
    check(result)
    return result


def dumps(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), allow_nan=False)
