"""Export a bridge proposal; resource UUIDs must be replaced by registered versions."""
import argparse
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))
from poligon_intelligence.demo import uid
from poligon_intelligence.models import dumps
from poligon_intelligence.scenarios import catalog


def platform_definition(scenario):
    if scenario.initial.orders:
        raise ValueError("Existing-order fixtures are currently unsupported by Runner/Environment initialization")
    return dict(schema_version=1, dataset_item_id=uid("item/" + scenario.id), title=scenario.goal,
        modality="text", environment_version_id=uid("environment.v1"), template_id="service-booking",
        template_version="1.0.0", initial_fixture={"clients": list(scenario.initial.clients.values()),
            "slots": list(scenario.initial.slots.values())}, actor_customer_ref=scenario.client_id,
        virtual_clock="2026-11-11T12:00:00+03:00", business_timezone="Europe/Moscow",
        client_goal=scenario.goal, client_known_facts={"intelligence_scenario": scenario.model_dump()},
        simulator_version_id=uid("simulator.v1"), fault_rules=scenario.fault_rules,
        assertions=[{"rule_id": "booking.count.v1", "required": True}],
        limits={"max_client_turns": scenario.max_turns, "max_tool_calls": 20, "session_duration_ms": 60000},
        seed_policy={"supported": True, "external_model_determinism": "not_guaranteed"})


def main():
    import jsonschema
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", default="output/platform-proposals")
    args = parser.parse_args()
    output = Path(args.output)
    output.mkdir(parents=True, exist_ok=True)
    from poligon_intelligence.models import loads
    schema = loads((ROOT / "contracts/upstream/platform/contracts/schemas/scenario.json").read_text(encoding="utf-8"))
    exported, unsupported = [], []
    for scenario in catalog().values():
        if scenario.initial.orders:
            unsupported.append(scenario.id)
            continue
        result = platform_definition(scenario)
        jsonschema.validate(result, schema, format_checker=jsonschema.FormatChecker())
        (output / f"{scenario.id}.json").write_text(dumps(result), encoding="utf-8")
        exported.append(scenario.id)
    print(dumps({"proposal_only": True, "resource_ids": "demo placeholders; must register actual versions", "exported": exported,
                 "unsupported_existing_order_initialization": unsupported}))


if __name__ == "__main__": main()
