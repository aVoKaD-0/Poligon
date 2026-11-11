## Изменение

Добавлен локальный текстовый MVP Intelligence: сценарный/модельный симулятор,
восемь синтетических сценариев, контролируемые варианты, проверки снимка и аудита
Environment, отдельные оценки качества, доказательства, группировка и метрики.
После сохранения с HTTP 504 ошибочный агент создаёт дубликат и получает fail;
исправленный читает состояние, оставляет одну заявку и получает pass. Нехватка
данных, ошибка симуляции и инфраструктурный сбой различаются в inconclusive.

Assessment соответствует схеме Platform. Общие контракты закреплены по commit/SHA.
Предложенный gRPC реализует Open/Next/Close/Evaluate и opt-in Struct validation.
SQLite сохраняет повторы и изолирует project/run/session/attempt.

## Проверка и ограничения

Команды и выполненные проверки: intelligence/VERIFICATION.md. Демо использует
имитаторы Environment/агента; локальный gRPC проверяется Python-клиентом. Реальные
Go Runner, NATS/Control/artifact registry, mTLS и финализация Environment ещё не
подключены. Live LLM не запускалась. Инициализация существующих заявок отсутствует
в Runner/Environment: change-address/reschedule/cancel пока локальные сценарии.

Изменения только в intelligence/. Точки согласования: intelligence/INTEGRATION.md.
Объединение с main требует командного ревью.
