"""Run from intelligence/: python scripts/generate_contracts.py"""
from pathlib import Path
import sys

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))


def main():
    from grpc_tools import protoc
    import grpc_tools
    from poligon_intelligence.models import Action, Assessment, Scenario, SessionMaterials
    from poligon_intelligence.analysis import Finding, FindingLabel, HumanScore, Measurements
    from poligon_intelligence.evaluation import QualityOutput
    include = Path(grpc_tools.__file__).parent / "_proto"
    source = ROOT / "contracts" / "proto"
    files = sorted(source.rglob("*.proto"))
    result = protoc.main(["grpc_tools.protoc", f"-I{source}", f"-I{include}",
        f"--python_out={ROOT / 'src'}", f"--grpc_python_out={ROOT / 'src'}", *map(str, files)])
    if result:
        raise SystemExit(result)
    target = ROOT / "contracts" / "schemas"
    target.mkdir(parents=True, exist_ok=True)
    import json
    for model in (Scenario, SessionMaterials, Assessment, Action, Finding, FindingLabel, HumanScore, Measurements, QualityOutput):
        (target / f"{model.__name__}.json").write_text(json.dumps(model.model_json_schema(), ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print("Generated protobuf bindings and v1 JSON schemas")


if __name__ == "__main__":
    main()
