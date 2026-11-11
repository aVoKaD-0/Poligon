# Состояние проекта до реализации Intelligence

Проверено 11.11.2026 по свежему clone `origin=https://github.com/aVoKaD-0/Poligon.git`.
Рабочая копия была чистой. Ветка `feature/ml-intelligence` создана от `origin/main`.

| Источник | Проверенная версия | Фактическое состояние |
|---|---|---|
| main | ba3fd808abce70226eabed9ffac650f0c8ad1780 | README компонентов, кода Intelligence нет |
| platform | 2fe2700620c946e236377c79886e511743d7608c | Go API, Control gRPC, задания оценки, artifact/manifest/assessment schemas |
| runner | 75f2454 | Go исполнитель, HTTP/WS агента, SimulatorFactory; бинарник без клиента Intelligence |
| Environment | c9932fea7eca48d6fccb2e06a997478c8c59da62 | Go CRM HTTP API, snapshots, audit, управляемые сбои, Verify |

AGENTS.md не найден. Содержимое связанных веток прочитано через git, ветки не объединялись.
В `contracts/upstream/` сохранены точные копии общих схем и исходный Control proto.
Это доступные контракты реализаций, а не свидетельство их командного утверждения.

Бриф v1.0 от 27.10.2026 прямо обозначает архитектуру предложением. В таблице ролей
проверки состояния есть у Environment, а README Intelligence поручает проверку
бизнес-результата Intelligence. Разделение MVP: Environment владеет состоянием,
аудитом и выполнением сбоев, Intelligence получает эти факты и оценивает их по
версионированным правилам. Environment Verify не заменяет независимое оценивание.

Не хватает согласованных схем snapshot/audit (пример Platform сейчас является
транспортной фикстурой, не CRM snapshot), аутентифицированной финализации Environment,
подключённого SimulatorFactory клиента, общей политики хранения/прав, реальных
человеческих меток, выбранного LLM и тарифов. Важное ограничение: Runner строго
принимает initial_fixture только с clients/slots, Environment InitSession также не
принимает orders. Смена адреса/перенос/отмена существующей заявки пока проверяются
на локальных fixture; их production validation явно отклоняется до bootstrap API.
Полный broker/lease/artifact worker
Intelligence пока не реализуется: MVP предоставляет отдельную функцию оценки и RPC.

Решения MVP: синтетические восемь сценариев; строгие локальные JSON-схемы;
SQLite для идемпотентных операций с изоляцией project/run/session/attempt;
сценарный симулятор и заменяемый модельный провайдер; фактические правила отдельно
от поведения и субъективных оценок. Предлагаемый provider-owned gRPC использует
уже существующий ExecutionContext. Операции симулятора следуют предложению Runner;
Struct validation subset следует явному предложению Platform.

Статусы assessment сохраняются: pass/fail/inconclusive. Категория причины и ошибки
инфраструктуры передаются в разрешённом completeness, simulation_validity отдельно.
Ошибка стенда не становится новым несовместимым verdict. Lifecycle Platform
(executed/evaluating/completed/failed/cancelled) не определяется нашим verdict.

Сначала выполняются локальные тесты и демонстрация на явно обозначенных имитаторах.
Полный прогон с Go, NATS, lease, artifact registry и mTLS требует совместной приёмки.
