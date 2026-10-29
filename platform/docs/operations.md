# Эксплуатация и восстановление

## Профили

Compose — локальный профиль с отдельными PostgreSQL database/credentials Platform. Другим сервисам нельзя выдавать DATABASE_URL Platform. Постоянные volumes: platform-db, platform-nats, platform-evidence. `docker compose down` сохраняет данные; удаление volumes является отдельной разрушительной операцией.

Production: APP_ENV=production, HTTPS PUBLIC_ORIGIN, настроенный OIDC, S3 TLS и mTLS Control/validator calls. Сервис завершает старт при небезопасном сочетании local/private конфигурации. Выпуск с local auth в публичный контур не поддерживается. Перед API поставить TLS reverse proxy с ограничением body и без буферизации SSE. Для issuer нужен общий DNS из браузера и контейнера. Сервисный сертификат Platform для исходящих вызовов должен иметь идентичность, принимаемую validators.

NATS/S3/PostgreSQL production credentials задаются оператором. Ограниченные NATS subject ACL — в deploy/nats.conf. Local MinIO root credentials выдаются только Platform; соседние workers используют grants, не root keys. Для production заменить их S3 principal с доступом только к bucket/namespace Platform. Не печатать signed URL и auth headers в proxy/access logs.

## Наблюдаемость

/livez проверяет процесс; /readyz — версию миграции и БД, показывает задержанный outbox. Недоступность брокера не теряет новые назначения: они остаются в outbox; operator log и метрики показывают задержку. Broker loop повторяет подключение, а supervisor продолжает работать по PostgreSQL.

/metrics с METRICS_TOKEN: HTTP requests/errors/duration sum, SSE connections, outbox pending/lag, queued/running sessions, retries, expired leases, stale results и quarantine. Настроить alert на outbox lag > 5 минут, растущую quarantine, queued age и частые expiry. Полный API p95 рассчитывать из trace/ingress histogram; встроенная метрика duration_sum сама p95 не вычисляет. OTLP traces включаются через OTEL_EXPORTER_OTLP_ENDPOINT.

Supervisor каждые 5 секунд проверяет lease/deadline, queue timeout, durable cancel и ограниченные retries. Execution lease 30 с, heartbeat 10 с, разговор по frozen limit + 30 с финализации. Evaluation deadline 120 с text / 300 с voice. Автоматического повтора дефекта агента нет.

Run сохраняется cancelled после отмены, даже при частично завершённых проверках; общий verdict=null. Дополнительная manual оценка не меняет исходный primary результат.

## Incident: NATS недоступен

1. Проверить состояние брокера и свободное место stream storage.
2. Проверить platform_outbox_pending/lag_seconds; не очищать outbox.
3. Восстановить NATS и ACL. Publisher повторит подтверждённые DB-команды с прежним message_id.
4. Проверить inbox/quarantine и актуальные attempt/job. Не публиковать новую команду с изменённым attempt вручную.

DiscardNew предотвращает незаметное вытеснение необработанных команд. Заполненный stream возвращает ошибку публикации, а outbox остаётся pending. Queued job старше 24 часов получает QUEUE_TIMEOUT. Переотправка после этого не даст старому worker действующий claim.

## Incident: исполнитель умер

Новые действия после expiry запрещены. Supervisor закрывает старую попытку и, если разрешено, назначает целый новый attempt с чистой средой. Старый результат сохраняется в stale_results, но не заменяет новый. Ошибка оценщика повторяет только оценку на том же manifest.

## Backup

Нужны pg_dump совместимой major версии и MinIO mc с заранее настроенным оператором alias. Папка backup должна быть новой.

```sh
python3 scripts/backup.py /secure-backups/platform-2026-10-30 --s3-source platform/poligon-evidence
```

Скрипт делает PostgreSQL custom dump и копирует bucket. Пароль БД передаётся дочернему процессу через env и не включается в command args. Не запускать одновременно purge-expired. При работающих producers S3 может содержать дополнительные объекты сверх snapshot БД; это безопасно. Объекты, уже присутствующие в dump registry, immutable и должны присутствовать в backup. Для строгой проверки backup приостановить новые назначения и дождаться завершения активных writes.

CREDENTIAL_MASTER_KEY хранить отдельно в защищённом хранилище секретов, с версией backup. S3 backup и БД без исходного ключа не позволяют расшифровать credentials.

## Restore drill

Восстановление выполнять в новом изолированном окружении и новой пустой platform_db. Рабочую БД эти инструкции не перезаписывают.

1. Создать новую БД/пользователя и отдельный bucket. Использовать pg_restore --no-owner с platform.dump в пустую БД; при ошибке остановиться, не стартовать Platform поверх частичной схемы.
2. Восстановить objects через mc mirror в новый bucket.
3. Указать DATABASE_URL, S3 endpoint/bucket и исходный CREDENTIAL_MASTER_KEY. Отключить доступ восстановленного экземпляра к рабочим очередям и настоящим агентам.
4. Запустить platform verify-artifacts: все зарегистрированные объекты должны совпасть по размеру и SHA-256.
5. Проверить авторизацию, membership, primary report, SSE snapshot/replay и чтение ciphertext credentials с исходным ключом. С другим ключом расшифровка должна завершаться ошибкой; тест на это есть в unit suite.
6. На отдельном тестовом NATS проверить outbox replay и lease supervisor. Копия активного run может повторно назначить работу, поэтому подключать её к рабочим agents/queues до решения оператора нельзя.
7. Зафиксировать длительность восстановления, найденные расхождения и версию БД/образов. Только после этого считать backup проверенным.

Первичная schema migration не откатывается удалением таблиц с пользовательскими данными. Откат deployment не меняет БД, а восстановление старой схемы выполняется через этот drill.

## Retention

Артефакты получают retain_until=30 дней. Очистка явная: platform purge-expired --apply. Команда резервирует только истёкшие объекты терминальных запусков без активных evaluation jobs, затем удаляет S3 и registry; reservation сохраняется при сбое. Новая manual оценка не стартует на зарезервированном/удалённом evidence. Удаление идёт батчами до 1000 объектов; повторять команду до завершения. Сессия с истёкшим evidence показывает completeness.status=expired.

Live SSE history терминальных runs хранится 30 дней. Старый cursor приводит к stream.reset; snapshot остаётся доступным. Browser signed URL действует максимум 60 секунд, upload grant — 10 минут.
