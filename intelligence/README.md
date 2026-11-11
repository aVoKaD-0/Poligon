# Intelligence — локальный текстовый MVP

Симуляция клиента, контролируемые вариации, проверка фактов Environment, отдельная
оценка диалога, доказательства, группировка дефектов и расчёт метрик. Работает без
внешних моделей, API-ключей и платных сервисов. Python 3.12, пакет 0.1.0.

**Статус:** локальный MVP. Демо использует явно обозначенные имитаторы Environment
и двух версий агента. Platform, Runner, CRM, NATS и frontend этим модулем не
заменяются. Полная интеграция с Go пока не проверена.

Исследование веток и брифа: [REPOSITORY_REVIEW.md](REPOSITORY_REVIEW.md).
Точки интеграции и ограничения: [INTEGRATION.md](INTEGRATION.md).
Разметка и метрики: [MEASUREMENTS.md](MEASUREMENTS.md).
Фактически выполненные проверки: [VERIFICATION.md](VERIFICATION.md).

## Установка на Windows / PowerShell

В этой среде доступен Python из комплекта Codex. Воспроизводимые команды:

```powershell
Set-Location 'C:\Users\User\Documents\ChatGPT\Poligon\poligon-repo\intelligence'
$python312 = 'C:\Users\User\.cache\codex-runtimes\codex-primary-runtime\dependencies\python\python.exe'
& $python312 -m venv --without-pip .venv
& $python312 -m pip --python .venv/Scripts/python.exe install -r requirements.lock
& $python312 -m pip --python .venv/Scripts/python.exe install --no-deps --no-build-isolation -e .
.\.venv\Scripts\python.exe -m unittest discover -s tests -v
.\.venv\Scripts\python.exe -m poligon_intelligence.cli demo --output output/demo
```

На обычном компьютере с CPython 3.12:

```powershell
py -3.12 -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.lock
.\.venv\Scripts\python.exe -m pip install --no-deps --no-build-isolation -e .
```

Linux/macOS: `python3.12 -m venv .venv`, затем `.venv/bin/python -m pip ...`;
во всех командах заменить `.\.venv\Scripts\python.exe` на `.venv/bin/python`.
Lock фиксирует также транзитивные зависимости проверенной среды Windows/CPython 3.12;
другие ОС отдельно не проверялись. Для одной бизнес-логики достаточно `pip install -e .`;
gRPC и jsonschema нужны для транспорта и полной проверки.

## Демонстрация «тайм-аут после сохранения»

```powershell
.\.venv\Scripts\python.exe -m poligon_intelligence.cli demo --output output/demo
```

| Вариант | Наблюдение | verdict / категория |
|---|---|---|
| buggy | сохранение, HTTP 504, повтор создания, две активные заявки | fail / agent_defect |
| fixed | сохранение, HTTP 504, чтение состояния, одна заявка | pass / success |
| missing-state | итогового состояния нет | inconclusive / insufficient_evidence |
| invalid-simulation | некорректная симуляция | inconclusive / invalid_simulation |
| infrastructure | отдельный сбой стенда | inconclusive / infrastructure_error |

`output/demo/<вариант>/` содержит materials.json, assessment.json и точные байты
артефактов `artifacts/<UUID>.json`. Проверка booking.count.v1 указывает на `/orders`
snapshot и `/events` audit: видны обе записи и 504 после сохранения. summary.json
помечен test_doubles=true. Это не сквозной запуск платформы.

```powershell
.\.venv\Scripts\python.exe -m poligon_intelligence.cli evaluate output/demo/buggy/materials.json --output output/reassessment.json
.\.venv\Scripts\python.exe -m poligon_intelligence.cli cluster output/demo/buggy/materials.json output/demo/fixed/materials.json --output output/clusters.json
```

## Сценарии и вариации

Каталог: create, change-address, reschedule, cancel, ambiguous, unavailable,
timeout-after-save и repeat. Условия и ожидаемые исходы: [SCENARIOS.md](SCENARIOS.md).
Даты фиксированные и вымышленные. Каждый сценарий
содержит цель, initial snapshot, ограничения, ожидаемый результат, допустимые исходы,
конечный скрипт, лимит ходов, family, ID/version и provenance. Fault rules только
описываются Intelligence; исполняет их Environment.

Смена адреса, перенос и отмена локально проверяются на существующих заявках.
Закреплённые Runner/Environment **не загружают начальные заявки**: для общего прогона
этих трёх сценариев нужно согласовать bootstrap/fixture API. Экспорт их отклоняет.

```powershell
.\.venv\Scripts\python.exe -m poligon_intelligence.cli scenarios --output output/scenarios
.\.venv\Scripts\python.exe -m poligon_intelligence.cli vary timeout-after-save --seed 42 --address 'Тестоград, Новая 9' --output output/variant.json
.\.venv\Scripts\python.exe scripts/export_platform_scenarios.py
```

Локальный generator выбирает разрешённые формулировки, либо согласованно меняет
запрошенный адрес в ожидаемом результате и репликах. Остальные параметры ограничены.
Цель и семейство сохраняются. В provenance записываются parent, seed, origin,
параметры и версия generator. Экспорт Platform — schema-valid предложение с demo
UUID, а не опубликованные ресурсы.

Python API: `next_turn(scenario, history, step, seed=..., provider=...)`. Сервис
добавляет ExecutionContext и сохранённую историю. Ответ — say/finish и metadata
валидности. Требующий уточнения шаг не раскрывается, пока не наблюдался вопрос.

## gRPC

```powershell
.\.venv\Scripts\python.exe scripts/generate_contracts.py
.\.venv\Scripts\python.exe -m poligon_intelligence.cli serve --insecure-local
```

В другом окне PowerShell из той же папки:

```powershell
.\.venv\Scripts\python.exe examples/grpc_client.py
```

Пример вызывает Open/Next, повторяет Next, оценивает fixed fixture и закрывает
симуляцию. Повтор возвращает исходный ответ, оценка — pass. Ctrl+C останавливает
сервер. По умолчанию 127.0.0.1:50053, БД output/intelligence.sqlite3.

Для варианта с аутентификацией установите INTELLIGENCE_TOKEN в обоих окнах и
запустите serve без --insecure-local. `.env.example` — образец, автоматической
загрузки `.env` нет. Реальные токены не сохранять в коде, отчётах или Git.
Сервер поддерживает только loopback; deployment mTLS/authorization требуют интеграции.

Proto: `contracts/proto/poligon/intelligence/v1/intelligence.proto`.
ExecutionContext импортируется из исходного Control proto. Python bindings и JSON
schemas сохраняются в Git. Генератор использует закреплённые grpcio-tools/protobuf.
Команда Go после согласования имён module/package:

```text
protoc -I contracts/proto --go_out=paths=source_relative:gen --go-grpc_out=paths=source_relative:gen poligon/control/v1/control.proto poligon/intelligence/v1/intelligence.proto
```

Версии Go plugins выбирает backend-команда. Генерация Go и подключение к Runner
пока не проверялись. Open/Next/Close/Evaluate — предложение API.

## Заменяемая модель

`providers.Provider` задаёт complete(system, data, schema, seed, timeout).
Опциональный OllamaProvider использует уже запущенную локальную Ollama с заранее
установленной моделью; автоматического скачивания моделей нет.

```powershell
# Заменить YOUR_INSTALLED_MODEL на имя уже установленной модели.
.\.venv\Scripts\python.exe -m poligon_intelligence.cli serve --insecure-local --ollama-model YOUR_INSTALLED_MODEL
.\.venv\Scripts\python.exe -m poligon_intelligence.cli evaluate output/demo/buggy/materials.json --ollama-model YOUR_INSTALLED_MODEL --output output/model-assessment.json
.\.venv\Scripts\python.exe -m poligon_intelligence.cli vary create --seed 7 --ollama-model YOUR_INSTALLED_MODEL --output output/model-variant.json
```

Для модельной симуляции Open получает mode=model. В MVP модель выбирает точную
реплику каталога: свободная генерация без надёжной проверки смысла цели не включена.
Модельные вариации также выбирают утверждённые парафразы. Неверная схема, intent,
раннее finish и неизвестный вариант фиксируются как ошибка симуляции. Лимиты ходов
проверяются независимо от модели. Duplicate JSON keys, NaN, NUL, неизвестные поля
и глубокая вложенность отклоняются.

Диалог передаётся как untrusted_data, системные правила фиксированы. Инструкции в
диалоге не могут изменить фактические правила и verdict по БД. Субъективный LLM
может ошибаться: его три оценки 1..5 находятся отдельно и требуют человеческой
калибровки. Без модели scores пусты, quality=not_evaluated.
Сохраняются model/settings/prompt_version/seed, токены и время при наличии.
Seed не гарантирует детерминированность внешней модели. Стоимость Ollama — unknown:
бесплатный API не доказывает нулевую стоимость вычислений. Live LLM не запускалась.
Адаптер следует официальному [Ollama API](https://github.com/ollama/ollama/blob/main/docs/api.md);
отмена — [правилам gRPC](https://grpc.io/docs/guides/cancellation/).

## Структура

```text
src/poligon_intelligence/  модели, симулятор, оценщик, сервис, gRPC, CLI, метрики
src/poligon/              сгенерированные protobuf bindings
contracts/proto/         исходный Control и предложенный Intelligence proto
contracts/schemas/       JSON Schema из моделей
contracts/upstream/      точные копии доступных общих контрактов
fixtures/                синтетические сценарии и демонстрационный формат разметки
tests/                   бизнес-правила, изоляция/повторы, метрики, реальные локальные RPC
examples/                gRPC клиент
```

## Git и публикация

Сейчас ветка и изменения находятся только на этом компьютере. Push не выполнялся.
После публикации origin/feature/ml-intelligence её увидят пользователи с доступом
к репозиторию. Для закрытого репозитория участникам нужен доступ. Pull Request
позволит команде проверить изменения до объединения с main.

Следующие команды предназначены для **отдельно разрешённой публикации**, сейчас
они не выполнялись. Включены только изменения Intelligence:

```powershell
Set-Location 'C:\Users\User\Documents\ChatGPT\Poligon\poligon-repo'
git switch feature/ml-intelligence
git status --short
git add intelligence
git diff --cached --check
git commit -m "feat(intelligence): add local text simulation and evidence-based evaluation MVP"
git push -u origin feature/ml-intelligence
gh pr create --repo aVoKaD-0/Poligon --base main --head feature/ml-intelligence --title "Intelligence: local text MVP" --body-file intelligence/PR_DESCRIPTION.md
```

GitHub CLI (`gh`) в PATH сейчас не обнаружен. После push можно открыть
[создание PR](https://github.com/aVoKaD-0/Poligon/compare/main...feature/ml-intelligence)
и вставить PR_DESCRIPTION.md. До публикации нужны настроенные Git identity и GitHub
authentication. Команда проверяет PR и общие контракты; участник с нужными правами
объединяет его на GitHub. В main ничего не объединялось, force push не использовался.
После merge: `git switch main`, затем `git pull --ff-only origin main`.

[О проекте](../README.md)
