# AI benchmark dataset v1

Одна строка JSONL — один размеченный случай контракта
`lidradar-ai-benchmark.v1`. Поля `id` уникальны, а `split` принимает только
`TRAIN`, `VALIDATION` или `GOLDEN`. В репозитории находятся только синтетические
данные без сообщений клиентов. Пустой `expectedFacts` является явной разметкой
случая без semantic facts и нужен для измерения false positives.

Golden-набор изменяется только отдельным ревью: после осознанного изменения
файла следует выполнить `sha256sum models/datasets/golden_v1.jsonl >
models/datasets/golden_v1.sha256`. Benchmark прекращает работу при несовпадении
контрольной суммы. Целевой полный набор — 300–500 размеченных случаев; текущие
четыре примера проверяют формат и runner, но не позволяют заморозить модель.

Запуск на AI Node:

```sh
go run ./backend/cmd/ai-benchmark \
  -endpoint http://127.0.0.1:8080/v1/chat/completions \
  -minimum-precision <утверждённое-значение> \
  -minimum-recall <утверждённое-значение> \
  -minimum-f1 <утверждённое-значение> \
  -minimum-exact-rate <утверждённое-значение> \
  -maximum-p95-ms <утверждённое-значение>
```

Значения gate в `Tasks.md` не заданы, поэтому runner требует передать их явно и
не подменяет продуктовое решение выдуманными defaults.
