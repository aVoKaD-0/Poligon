from contextlib import redirect_stdout
import io
import os
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

from poligon_intelligence import cli
from poligon_intelligence.demo import uid
from poligon_intelligence.grpc_server import build_server, platform_scenario
from poligon_intelligence.models import dumps, loads
from poligon_intelligence.service import Intelligence, Store

ROOT = Path(__file__).resolve().parents[1]


class CliTests(unittest.TestCase):
    def call(self, *args):
        with patch.object(sys, "argv", ["intelligence", *map(str, args)]), redirect_stdout(io.StringIO()):
            cli.main()

    def test_documented_cli_flow(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as tmp:
            root = Path(tmp)
            self.call("demo", "--output", root / "demo")
            self.call("scenarios", "--output", root / "scenarios")
            self.call("vary", "timeout-after-save", "--seed", 42, "--address", "Тестоград, Новая 9", "--output", root / "variant.json")
            self.call("evaluate", root / "demo/buggy/materials.json", "--output", root / "report.json")
            self.assertEqual(loads((root / "report.json").read_text(encoding="utf-8"))["verdict"], "fail")
            self.call("cluster", root / "demo/buggy/materials.json", root / "demo/fixed/materials.json", "--output", root / "clusters.json")
            self.call("metrics", ROOT / "fixtures/annotations.demonstration.json", "--output", root / "metrics.json")
            self.assertIsNone(loads((root / "metrics.json").read_text())["precision"]["value"])
            self.call("freeze", "--output", root / "frozen.json")
            self.call("verify-frozen", root / "frozen.json")

    def test_platform_export_is_schema_valid_and_bridge_accepts_supported_only(self):
        import jsonschema
        script = runpy.run_path(str(ROOT / "scripts/export_platform_scenarios.py"))
        from poligon_intelligence.scenarios import catalog
        schema = loads((ROOT / "contracts/upstream/platform/contracts/schemas/scenario.json").read_text())
        for s in catalog().values():
            if s.initial.orders:
                with self.assertRaises(ValueError): script["platform_definition"](s)
            else:
                value = script["platform_definition"](s)
                jsonschema.validate(value, schema)
                self.assertEqual(platform_scenario(value), s.model_dump())

    def test_documented_grpc_example_executes(self):
        with tempfile.TemporaryDirectory(dir=Path(__file__).parent) as tmp:
            server = build_server(Intelligence(Store(Path(tmp) / "example.sqlite3")), insecure_local=True)
            # The example's default endpoint is tested without requiring a fixed free port.
            port = server.add_insecure_port("127.0.0.1:0")
            server.start()
            import grpc
            real_channel = grpc.insecure_channel
            try:
                with patch.dict(os.environ, {"INTELLIGENCE_TOKEN": ""}), patch.object(grpc, "insecure_channel",
                    side_effect=lambda _: real_channel(f"127.0.0.1:{port}")), redirect_stdout(io.StringIO()) as stream:
                    runpy.run_path(str(ROOT / "examples/grpc_client.py"), run_name="__main__")
                self.assertIn("Assessment: pass success", stream.getvalue())
            finally:
                server.stop(0).wait()


if __name__ == "__main__": unittest.main()
