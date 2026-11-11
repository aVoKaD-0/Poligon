# Фактически выполненная проверка

11.11.2026, Windows, CPython 3.12.14, зависимости requirements.lock.
Ветка feature/ml-intelligence от origin/main (ba3fd80). Проверены также исходники
origin/platform, origin/runner, origin/Environment; они не объединялись.

## Результат

`python -m unittest discover -s tests -v`: **49 tests, OK**.
Запущен полный набор, включая реальные локальные gRPC server/client на случайных
loopback портах, без внешней сети и без платных сервисов. Первоначальный запуск
был заблокирован ограничением сокетов среды; разрешённый повтор прошёл.

- Восемь сценариев: создание, изменение адреса/времени, отмена, неоднозначность,
  недоступное окно, тайм-аут после сохранения, повтор операции.
- Дубликат, неверные клиент/адрес/дата/время, защищённые поля/клиенты и промежуточное
  запрещённое изменение; отсутствие снимка, неполный audit, invalid/unknown simulation.
- Проверки SHA, счётчиков, scope и формы payload; конфликт/перестановка/повтор событий;
  легальные разрывы session_seq Runner и отдельный индекс покрытия.
- Open/Next/Close/Evaluate, конкурентные одинаковые запросы, конфликт payload,
  сохранение ответов при перезапуске, изоляция project/run/session/attempt.
- Аутентификация, обязательный deadline, отмена реального RPC; rollback шага,
  который уже начал модельную генерацию. Терминальная ошибка модели сохраняется.
- Схема Platform assessment и разрешимость всех ссылок evidence пяти demo случаев.
  JSON schemas совпадают с исходными моделями; SHA исходных контрактов закреплены.
- Модельные test doubles: правильный/невалидный ответ, смена цели, преждевременное
  завершение, модельный выбор вариантов, субъективная оценка не отменяет дубликат.
- Группировка без потери evidence, recall/precision по тестовым данным, exclusion
  demonstration labels, agreement/variance/duration/partial costs, frozen family split.
- Все документированные основные CLI операции; gRPC пример клиента; экспорт и
  Struct validation bridge, отказ от неподдерживаемых initial orders.

Дополнительно отдельно выполнены:

```text
python scripts/snapshot_upstream.py
python scripts/generate_contracts.py
python scripts/export_platform_scenarios.py
python -m poligon_intelligence.cli demo --output output/demo
python -m poligon_intelligence.cli metrics fixtures/annotations.demonstration.json --output output/metrics-format-demo.json
python -m poligon_intelligence.cli cluster output/demo/buggy/materials.json output/demo/fixed/materials.json --output output/clusters.json
python -m compileall -q src scripts examples
pip check
```

Компиляция модулей прошла; pip check: No broken requirements found.
git diff --check не обнаружил ошибок в изменении отслеживаемого README.

## Наблюдаемый итог демонстрации

| case | verdict | category |
|---|---|---|
| buggy | fail | agent_defect |
| fixed | pass | success |
| missing-state | inconclusive | insufficient_evidence |
| invalid-simulation | inconclusive | invalid_simulation |
| infrastructure | inconclusive | infrastructure_error |

Это результаты исполнения тестовых адаптеров из demo.py. HTTP 504 и изменение
состояния моделируются раздельно. Исходные артефакты и отчёты находятся в output/demo;
они воспроизводятся командой выше, output не коммитится.

## Что не проверено

Не запускались Go Platform/Runner/Environment, NATS, PostgreSQL, artifact registry,
production authorization/mTLS, Go bindings и живые внешние/локальные LLM. Есть только
model test doubles и реализованный опциональный Ollama adapter. Нет независимой
человеческой разметки и итогового скрытого набора; численные показатели продукта,
стоимость, нагрузка и полная интеграция не заявляются. Три existing-order сценария
требуют bootstrap API. Никакой push/PR/merge не выполнялся.
