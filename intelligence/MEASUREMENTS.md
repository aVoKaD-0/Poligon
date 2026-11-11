# Метрики и независимая разметка

CLI metrics принимает Measurements JSON. Контракты: analysis.py и contracts/schemas.
Пустой знаменатель даёт null. Тестовые вычисления не являются измерением продукта.

- known_defects: defect_id, case_id, family, rule_id. Независимый реестр действительно
  внесённых дефектов. Recall = найденные совпадения case/family/rule / все известные
  дефекты. Сценарий со сбоем без дефекта исправленного агента не входит в знаменатель.
- findings: finding_id, case_id, family, rule_id, expected, actual, reason, evidence.
  Кластеризация — точное совпадение family и rule_id с версией. Все исходные находки
  и доказательства сохраняются. Это базовая группировка, не доказательство общей
  первопричины. Два правила об одном дубликате могут образовать два кластера.
- finding_labels: finding_id, is_defect, reviewer_id, independent, demonstration.
  Precision считает только independent=true, demonstration=false; непроверенные
  находки показаны отдельно. Несколько меток одной находки требуют внешнего
  разрешения разногласий. Самооценка LLM не считается независимой проверкой.
- human_scores: case_id, criterion, score 1..5, reviewer_id, independent, demonstration.
  По каждому case/criterion нужен один согласованный человеческий балл. Для каждого
  модельного повтора считаются n, exact agreement, MAE и квадратично взвешенная
  Cohen kappa. При нулевом ожидаемом расхождении kappa=null. Повторы одного case
  зависимы; это не доверительный интервал качества на независимых примерах.
- repeats: case_id, repeat_id, verdict, model_scores, duration_ms, cost_minor, currency.
  Считаются pass_rate, выборочная дисперсия pass/fail и standard deviation каждого
  score, число исключённых inconclusive. Один повтор не даёт дисперсию. case_id
  объединяет только одинаковые версии агента, среды, симулятора, оценщика, сценария
  и seed. Это обязанность поставщика данных, не вывод из текста диалога.
- Длительность — средняя по известным наблюдениям с числом измерений. Стоимость —
  известные суммы по валютам, статус complete/partial/unknown и число неизвестных.
  Неизвестные расходы не заменяются нулём; валюты не складываются и не конвертируются.

fixtures/annotations.demonstration.json содержит только вымышленный пример формы:
demonstration=true, independent=false. Это **не человеческая разметка**.

```powershell
.\.venv\Scripts\python.exe -m poligon_intelligence.cli metrics fixtures/annotations.demonstration.json --output output/metrics-format-demo.json
.\.venv\Scripts\python.exe -m poligon_intelligence.cli freeze --seed 7 --output output/development-split.json
.\.venv\Scripts\python.exe -m poligon_intelligence.cli verify-frozen output/development-split.json
```

freeze делит по семействам, сохраняет ID/version/hash каждого сценария и hash
manifest. timeout-after-save и repeat всегда в одной части: family=retry-duplicate.
Повтор записи существующего manifest запрещён. verify-frozen отклоняет смену
членства/содержимого/версии и утечку семейства между tuning и holdout.

Этот каталог уже использовался при разработке: development split не является
скрытым итоговым набором. Для настоящей оценки независимый участник заранее готовит
новые семейства, фиксирует отдельные файлы до запуска:

```powershell
.\.venv\Scripts\python.exe -m poligon_intelligence.cli freeze --scenarios unseen-case-1.json unseen-case-2.json --seed 19 --output final-set.json
.\.venv\Scripts\python.exe -m poligon_intelligence.cli verify-frozen final-set.json --scenarios unseen-case-1.json unseen-case-2.json
```

Не настраивать правила на holdout. Сохранить версии, запуски, manifest и независимые
метки; перед метриками повторить verify-frozen. Расчёт metrics сам по себе не
доказывает соблюдение протокола: доступ к holdout, время фиксации и независимость
людей контролирует владелец эксперимента. Реальные recall/precision/agreement,
стоимость и нагрузочные показатели этой поставкой не заявляются.
