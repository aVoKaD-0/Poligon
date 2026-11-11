"""Transactional, restart-safe local idempotency. Transport is intentionally separate."""
from contextlib import closing
import hashlib
from pathlib import Path
import sqlite3
import time
from uuid import uuid4

from .evaluation import evaluate
from .models import Message, Scenario, Scope, SessionMaterials, dumps, loads
from .simulator import next_turn
from .models import Action
from .providers import ModelOutputError


class Conflict(ValueError):
    pass


class Missing(LookupError):
    pass


class Cancelled(TimeoutError):
    pass


class Call:
    def __init__(self, timeout=10, active=lambda: True):
        self.deadline = time.monotonic() + min(timeout, 30)
        self.active = active

    def check(self):
        if not self.active():
            raise Cancelled("request cancelled")
        if time.monotonic() >= self.deadline:
            raise TimeoutError("request deadline exceeded")

    def remaining(self):
        self.check()
        return max(0.001, self.deadline - time.monotonic())


class Store:
    def __init__(self, path):
        self.path = str(path)
        Path(self.path).parent.mkdir(parents=True, exist_ok=True)
        with closing(self.connect()) as db:
            db.execute("PRAGMA journal_mode=WAL")
            db.executescript("""
                CREATE TABLE IF NOT EXISTS requests (
                    key TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, response TEXT NOT NULL);
                CREATE TABLE IF NOT EXISTS simulations (
                    scope TEXT PRIMARY KEY, id TEXT UNIQUE NOT NULL,
                    definition TEXT NOT NULL, state TEXT NOT NULL);
            """)

    def connect(self):
        return sqlite3.connect(self.path, timeout=0.05, isolation_level=None)

    def execute(self, key, payload, operation, call):
        fingerprint = hashlib.sha256(dumps(payload).encode()).hexdigest()
        with closing(self.connect()) as db:
            while True:
                call.check()
                try:
                    db.execute("BEGIN IMMEDIATE")
                    break
                except sqlite3.OperationalError as exc:
                    if "locked" not in str(exc):
                        raise
            try:
                found = db.execute("SELECT fingerprint,response FROM requests WHERE key=?", (key,)).fetchone()
                if found:
                    if found[0] != fingerprint:
                        raise Conflict("idempotency key/step reused with different input")
                    result = loads(found[1])
                else:
                    result = operation(db)
                    call.check()  # Deadline/cancellation rolls back both state and response.
                    db.execute("INSERT INTO requests VALUES (?,?,?)", (key, fingerprint, dumps(result)))
                call.check()
                db.commit()
                return result
            except BaseException:
                db.rollback()
                raise


class Intelligence:
    def __init__(self, store: Store, provider=None):
        self.store, self.provider = store, provider

    def open(self, scope: Scope, request_id: str, scenario: Scenario, seed: int = 0,
             mode="scripted", call=None):
        if mode not in {"scripted", "model"} or (mode == "model" and self.provider is None):
            raise ValueError("requested simulator mode unavailable")
        if type(seed) is not int or abs(seed) > 2**53 - 1:
            raise ValueError("seed exceeds JSON safe integer range")
        definition = dumps({"scenario": scenario.model_dump(), "seed": seed, "mode": mode})
        def operation(db):
            found = db.execute("SELECT id,definition FROM simulations WHERE scope=?", (scope.key(),)).fetchone()
            if found:
                if found[1] != definition:
                    raise Conflict("attempt already opened with another definition")
                return {"simulation_id": found[0], "capability_version": "text.v1", "seed_support": "scripted_exact_model_best_effort"}
            sim_id = str(uuid4())
            db.execute("INSERT INTO simulations VALUES (?,?,?,?)", (scope.key(), sim_id, definition,
                       dumps({"step": 0, "history": [], "closed": False})))
            return {"simulation_id": sim_id, "capability_version": "text.v1", "seed_support": "scripted_exact_model_best_effort"}
        return self.store.execute(f"open:{scope.key()}:{request_id}", loads(definition), operation, call or Call())

    def _simulation(self, db, scope, simulation_id):
        row = db.execute("SELECT definition,state FROM simulations WHERE scope=? AND id=?", (scope.key(), simulation_id)).fetchone()
        if not row:
            raise Missing("simulation not found in this execution scope")
        return loads(row[0]), loads(row[1])

    def next(self, scope, request_id, simulation_id, step, agent_messages, call=None):
        call = call or Call()
        if type(step) is not int or step < 0 or len(agent_messages) > 16:
            raise ValueError("invalid step/message count")
        messages = [Message(role="agent", text=m) for m in agent_messages]
        if sum(len(m.encode()) for m in agent_messages) > 24 * 1024:
            raise ValueError("agent messages exceed limit")
        payload = {"agent_messages": agent_messages, "step": step}
        def operation(db):
            definition, state = self._simulation(db, scope, simulation_id)
            if state["closed"] or state["step"] != step:
                raise Conflict("closed simulation or out-of-order step")
            history = [Message.model_validate(m) for m in state["history"]] + messages
            try:
                action, metadata = next_turn(Scenario.model_validate(definition["scenario"]), history, step,
                    seed=definition["seed"], provider=self.provider if definition["mode"] == "model" else None,
                    timeout=call.remaining())
            except ModelOutputError:
                # Pin invalid simulation as a terminal response, including on retries/restarts.
                # Never include raw model output in errors or logs.
                action = Action(action="finish", reason="invalid_simulation")
                metadata = {"validity": "invalid", "reason": "model output violated schema or scenario"}
            if action.action == "say":
                history.append(Message(role="client", text=action.text))
            state.update(step=step + 1, history=[m.model_dump() for m in history], closed=action.action == "finish")
            db.execute("UPDATE simulations SET state=? WHERE scope=?", (dumps(state), scope.key()))
            return {"simulation_id": simulation_id, "step": step, "action": action.model_dump(), "metadata": metadata}
        # Same simulation+step is idempotent even when Runner changes its request UUID.
        return self.store.execute(f"next:{scope.key()}:{simulation_id}:{step}", payload, operation, call)

    def close(self, scope, request_id, simulation_id, call=None):
        def operation(db):
            _, state = self._simulation(db, scope, simulation_id)
            state["closed"] = True
            db.execute("UPDATE simulations SET state=? WHERE scope=?", (dumps(state), scope.key()))
            return {"closed": True}
        return self.store.execute(f"close:{scope.key()}:{simulation_id}", {}, operation, call or Call())

    def evaluate(self, materials: SessionMaterials, request_id, call=None, quality=False):
        if quality and self.provider is None:
            raise ValueError("quality provider unavailable")
        key = f"evaluate:{materials.context.key()}:{materials.evaluation_id}"
        return self.store.execute(key, {"materials": materials.model_dump(), "quality": quality},
            lambda _: evaluate(materials, self.provider if quality else None).model_dump(), call or Call())
