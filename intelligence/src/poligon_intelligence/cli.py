import argparse
from pathlib import Path

from .analysis import Measurements, cluster, findings, freeze, metrics, verify_frozen
from .demo import run_demo
from .evaluation import evaluate
from .models import Assessment, Scenario, SessionMaterials, dumps, loads
from .scenarios import catalog, vary, vary_model


def write(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(dumps(value) + "\n", encoding="utf-8")


def read(path):
    return loads(Path(path).read_text(encoding="utf-8"))


def main():
    parser = argparse.ArgumentParser(description="Intelligence local text MVP")
    sub = parser.add_subparsers(dest="command", required=True)
    demo = sub.add_parser("demo")
    demo.add_argument("--output", default="output/demo")
    scenarios = sub.add_parser("scenarios")
    scenarios.add_argument("--output", default="output/scenarios")
    variation = sub.add_parser("vary")
    variation.add_argument("scenario")
    variation.add_argument("--seed", type=int, required=True)
    variation.add_argument("--address")
    variation.add_argument("--ollama-model")
    variation.add_argument("--output", required=True)
    assessment = sub.add_parser("evaluate")
    assessment.add_argument("materials")
    assessment.add_argument("--output", required=True)
    assessment.add_argument("--ollama-model")
    measure = sub.add_parser("metrics")
    measure.add_argument("input")
    measure.add_argument("--output", required=True)
    grouping = sub.add_parser("cluster")
    grouping.add_argument("materials", nargs="+")
    grouping.add_argument("--output", required=True)
    fixing = sub.add_parser("freeze")
    fixing.add_argument("--output", required=True)
    fixing.add_argument("--seed", default=0, type=int)
    fixing.add_argument("--scenarios", nargs="+")
    verify = sub.add_parser("verify-frozen")
    verify.add_argument("manifest")
    verify.add_argument("--scenarios", nargs="+")
    server = sub.add_parser("serve")
    server.add_argument("--bind", default="127.0.0.1:50053")
    server.add_argument("--db", default="output/intelligence.sqlite3")
    server.add_argument("--insecure-local", action="store_true")
    server.add_argument("--ollama-model")
    args = parser.parse_args()
    if args.command == "demo":
        print(dumps({"test_doubles": True, "cases": run_demo(Path(args.output))}))
    elif args.command == "scenarios":
        for key, scenario in catalog().items():
            write(Path(args.output) / f"{key}.json", scenario.model_dump())
    elif args.command == "vary":
        scenario = catalog()[args.scenario] if args.scenario in catalog() else Scenario.model_validate(read(args.scenario))
        if args.ollama_model:
            if args.address:
                parser.error("--address is only supported by seeded generation")
            from .providers import OllamaProvider
            result = vary_model(scenario, OllamaProvider(args.ollama_model), args.seed)
        else:
            result = vary(scenario, args.seed, args.address)
        write(args.output, result.model_dump())
    elif args.command == "evaluate":
        from .providers import OllamaProvider
        provider = OllamaProvider(args.ollama_model) if args.ollama_model else None
        write(args.output, evaluate(SessionMaterials.model_validate(read(args.materials)), provider).model_dump())
    elif args.command == "metrics":
        write(args.output, metrics(Measurements.model_validate(read(args.input))))
    elif args.command == "cluster":
        items = []
        for path in args.materials:
            material = SessionMaterials.model_validate(read(path))
            items.extend(findings(material.context.session_id, material.scenario.family, evaluate(material)))
        write(args.output, cluster(items))
    elif args.command == "freeze":
        dataset = [Scenario.model_validate(read(p)) for p in args.scenarios] if args.scenarios else list(catalog().values())
        freeze(dataset, Path(args.output), args.seed)
    elif args.command == "verify-frozen":
        dataset = [Scenario.model_validate(read(p)) for p in args.scenarios] if args.scenarios else list(catalog().values())
        verify_frozen(read(args.manifest), dataset)
        print("Frozen scenario set verified")
    elif args.command == "serve":
        from .grpc_server import serve
        from .providers import OllamaProvider
        serve(args.bind, args.db, args.insecure_local, OllamaProvider(args.ollama_model) if args.ollama_model else None)


if __name__ == "__main__":
    main()
