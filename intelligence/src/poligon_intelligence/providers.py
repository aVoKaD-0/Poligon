"""Replaceable model interface and optional local Ollama adapter (not required by MVP)."""
from dataclasses import dataclass
from typing import Any, Protocol
import time
import urllib.request

from .models import Model, dumps, loads


@dataclass(frozen=True)
class Completion:
    content: str
    model: str
    settings: dict[str, Any]
    input_tokens: int | None = None
    output_tokens: int | None = None
    cost_minor: int | None = None


class Provider(Protocol):
    def complete(self, *, system: str, data: dict, schema: dict, seed: int | None,
                 timeout: float) -> Completion: ...


class ModelOutputError(ValueError):
    """Invalid simulation/judge output, never an agent defect."""


def generate(provider: Provider, output: type[Model], *, system: str, data: dict,
             seed: int | None = None, timeout: float = 10) -> tuple[Model, dict]:
    started = time.monotonic()
    response = provider.complete(system=system, data=data, schema=output.model_json_schema(), seed=seed, timeout=timeout)
    try:
        validated = output.model_validate(loads(response.content))
    except (ValueError, TypeError, RecursionError) as exc:
        raise ModelOutputError("model response does not satisfy schema") from exc
    return validated, dict(model=response.model, settings=response.settings, seed=seed,
        input_tokens=response.input_tokens, output_tokens=response.output_tokens,
        cost_minor=response.cost_minor, cost_status="known" if response.cost_minor is not None else "unknown",
        duration_ms=(time.monotonic() - started) * 1000)


class OllamaProvider:
    """Explicit local endpoint, bounded nonstreaming response; no secrets or paid API."""
    def __init__(self, model: str, temperature: float = 0):
        self.model, self.temperature = model, temperature

    def complete(self, *, system, data, schema, seed, timeout):
        options = {"temperature": self.temperature, "num_predict": 512}
        if seed is not None:
            options["seed"] = seed
        body = dict(model=self.model, stream=False, format=schema, options=options,
                    messages=[{"role": "system", "content": system},
                              {"role": "user", "content": dumps({"untrusted_data": data})}])
        request = urllib.request.Request("http://127.0.0.1:11434/api/chat", data=dumps(body).encode(),
                                         headers={"Content-Type": "application/json"})
        # Disable redirects and proxy inheritance: transcript never leaves the configured local endpoint.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, *args, **kwargs):
                raise ValueError("model redirects forbidden")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        with opener.open(request, timeout=min(timeout, 30)) as response:
            raw = response.read(1_048_577)
        if len(raw) > 1_048_576:
            raise ModelOutputError("model response too large")
        value = loads(raw)
        return Completion(value["message"]["content"], self.model, options,
                          value.get("prompt_eval_count"), value.get("eval_count"))
