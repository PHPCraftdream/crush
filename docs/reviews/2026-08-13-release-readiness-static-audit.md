# Статический аудит готовности к релизу — 2026-08-13

## Резюме

**Вердикт: NO-GO.** Последняя серия исправлений закрыла несколько реальных
гонок и один явный deadlock, но текущий `main` всё ещё содержит четыре
release-blocker сценария:

1. cross-process interrupt теряет немедленное продолжение в короткоживущем
   `crush run`;
2. durable-вызов после compaction может быть успешно `Ack`-нут без выполнения
   обязательного продолжения;
3. lease-watchdog отсчитывает безопасный дедлайн не от фактически записанного
   срока lease и при медленном успешном `Renew` снова допускает двойное
   исполнение;
4. crash между claim и переносом orphan-outbox оставляет принятую работу в
   `processing` навсегда.

Классического цикла вида `mutex A -> mutex B -> mutex A` в просмотренных
изменениях не найдено. Однако наблюдаемые пользователем «фризы» не обязаны быть
классическим deadlock: ниже остаются потеря runnable-продолжения, вечное
состояние `processing`, неограниченный join ticker-а и lease race.

## Область и метод

- Точка ревью: `f9a70d045973ae9c8b09288d928ada70c6f2cc1a` (`main`).
- База: `3a145e60b3a0b790456667d7fd9eafe58f8d6fad`.
- Диапазон: 33 коммита, 48 изменённых файлов.
- Просмотрены история Git, production-код и связанные тесты/дизайн-документы.
- Основной фокус: `internal/agent`, mailbox, interrupt/inject, durable queue,
  orphan outbox, checkpointing, config snapshots и shutdown/lifecycle.
- По требованию ревью выполнялось только статически: тесты, сборка, lint и race
  detector не запускались.
- Рабочее дерево до ревью уже содержало чужие изменения
  (`web/dist/.gitkeep`, `dev/`); они не изменялись.

## Блокирующие находки

### P0-1. Durable interrupt отменяет текущий turn, но не продолжает его в том же процессе

`handleInterruptTick` помечает call как `FromDurableQueue=true`, атомарно
переносит inject в `session_run_queue`, затем вызывает `InterruptAndReplace`
(`internal/agent/coordinator.go:2474-2522`). Для такого call mailbox намеренно
не устанавливает `mb.replacement` (`internal/agent/mailbox.go:815-831`), чтобы
не получить двойное исполнение.

Это закрывает прежнее double ownership, но удаляет единственный live-handoff:
после отмены `sessionAgent.Run` завершает цикл, когда `runTurn` возвращает
`hasNext=false` (`internal/agent/agent.go:1601-1605`). Продолжение остаётся
только pending-строкой durable queue. В долгоживущем UI её позднее возьмёт
pump; короткоживущий `crush run` может сразу перейти к shutdown, а `Stop()` не
дренирует новые pending-строки. Пользователь видит принятый interrupt, который
не выполняется до следующего запуска процесса.

Это уже подтверждено amendment-ом в
`docs/reviews/2026-08-13-release-gate.md:14-32`, но на текущем HEAD исправления
нет. Комментарии `internal/agent/coordinator.go:2381-2391` и `:2415-2418`,
утверждающие, что replacement выполняется внутри того же `Coordinator.Run`,
теперь неверны.

**Что исправить:** определить ровно одного владельца исполнения, но сохранить
live-handoff для активного dispatcher-а. Durable row должна быть fence/источником
recovery, а не причиной завершить текущий runner без передачи следующего turn.
Нужен end-to-end сценарий busy `crush run` + cross-process interrupt + немедленное
завершение первой генерации + выполнение второй в том же процессе + ровно один
terminal outcome.

### P0-2. Post-compaction continuation durable-вызова молча теряется

При `shouldSummarize` и незавершённых tool calls `runTurn` меняет prompt и
вызывает `mb.submit(call, nil)` (`internal/agent/agent.go:3241-3265`). Если
исходный call пришёл из pump, `FromDurableQueue=true`. Guard в
`mailbox.submit` не добавляет такие calls в `mb.submitted`
(`internal/agent/mailbox.go:371-382`). Возвращаемое значение `submit` здесь
игнорируется.

Финальный drain не видит работы и возвращает `hasNext=false`
(`internal/agent/agent.go:3293-3310`). `Coordinator.Run` возвращает успех, после
чего pump удаляет durable row через `AckRunQueueEntry`
(`internal/session/run_queue_pump.go:1124-1125`, `:1163-1175`). Обязательное
продолжение после compaction потеряно без retry и без terminal error.

Дефект уже описан в
`docs/plans/2026-08-12-state-machine-unification-plan.md:36-43` и `:100-105`,
но не закрыт production-кодом.

**Что исправить:** внутреннее продолжение одного исполнения не должно снова
проходить через внешний acceptance guard. Возвращать его непосредственно как
`next, hasNext`, либо ввести отдельный internal-continuation primitive с тем же
durable execution identity. Pump может `Ack` только после завершения всей
цепочки, включая post-compaction turn.

### P0-3. Watchdog может сработать позже фактического истечения lease

Renewal сначала вычисляет `newExpiresAt := time.Now().Add(TTL).Unix()`, затем
делает DB-вызов (`internal/session/run_queue_pump.go:1083-1086`). После
успешного возврата он сохраняет в watchdog **время завершения** вызова как
`lastSuccessfulRenewal` (`:1091-1097`). Watchdog затем считает дедлайн как
`lastSuccessfulRenewal + TTL - safetyMargin` (`:1015-1027`).

Но значение в БД было вычислено от времени **начала** вызова. Если успешный
`RenewRunQueueLease` занял `D`, watchdog считает lease моложе реального на `D`.
При `D > safetyMargin` он отменит исполнение уже после фактического expiry.
Другой pump успеет выполнить `CleanupExpiredLeases`, взять строку и запустить
дубликат, пока старый provider/tool ещё работает. Индивидуальный timeout это не
закрывает: он равен оставшемуся safe budget и допускает успешный медленный вызов
дольше safety margin.

**Что исправить:** watchdog должен хранить абсолютный `lease_expires_at`,
который действительно был записан, и сравнивать `now >= expiresAt-margin`.
Начальную границу также следует брать из leased row, а не из локального
`time.Now()`. Альтернатива — вычислять новый expiry внутри SQL относительно
момента commit-а и возвращать его вызывающему коду. Нужен тест с успешным, но
искусственно задержанным `Renew`, где задержка превышает margin.

Даже после этого строгий exactly-once потребует fencing token перед каждым
persistent side effect: provider/tool может не остановиться мгновенно после
cancel. Код честно документирует этот остаточный риск в
`internal/session/run_queue_pump.go:979-991`.

### P0-4. Orphan outbox не восстанавливает строки `processing` после crash

State machine таблицы — `pending -> processing -> done|failed`, но в ней нет
owner, lease или claim deadline
(`internal/db/migrations/20260812000001_add_orphan_call_outbox.sql:23-41`).
Drain выбирает только `pending`, затем отдельно claim-ит строку, отдельно
enqueue-ит её в main queue и отдельно удаляет
(`internal/session/run_queue_pump.go:593-618`, `:623-681`). SQL также сканирует
только `status='pending'` (`internal/db/sql/orphan_outbox.sql:16-31`).

Исправлен обычный transient-error путь: он возвращает строку из `processing` в
`pending`. Но crash/kill между claim и enqueue/release оставляет строку в
`processing` навсегда. Ни один recovery scan её больше не увидит. Более того,
`TestOrphanOutbox_ClaimProtection` закрепляет это поведение, ожидая, что после
нового pump строка останется `processing`
(`internal/session/p0_3_orphan_outbox_drain_test.go:76-124`).

**Что исправить:** поскольку обе таблицы находятся в одной SQLite DB,
предпочтителен один transaction: idempotent insert в `session_run_queue` +
delete из outbox. Если разделение сохраняется, `processing` обязан иметь
`claimed_by/claim_expires_at` и recovery expired claims. Тест должен имитировать
crash после claim и доказывать восстановление новым pump.

## Высокий приоритет, но не отдельные P0

### P1-1. Config snapshot всё ещё не является сквозным snapshot-ом операции

Исправления вокруг `Snapshot()` полезны, но несколько torn-read путей остались:

- `buildAgentModels` берёт `cfg` один раз, но решение о worker-модели принимает
  через `workerSubAgentActive()`, который повторно читает текущий
  `c.cfg.Config()` (`internal/agent/coordinator.go:1940-1950`, `:1961-1981`).
  Reload между чтениями может заставить индексировать worker slot из другой
  generation, вплоть до zero-value model и ошибочного provider lookup.
- `resolveSessionModels` строит models и prefix из captured `cfg`, но system
  prompt строит через live `c.cfg` и ещё один live worker predicate
  (`internal/agent/coordinator.go:600-718`). Один pinned call может смешать
  model/prefix одной generation и prompt другой.
- 401 rebuild сначала вызывает `resolveSessionModels`, затем отдельно берёт
  новый `Snapshot()` для provider options
  (`internal/agent/coordinator.go:1023-1057`). Reload между ними смешивает model
  одной generation и credentials/options другой; комментарий о
  «consistent snapshot» неточен.

**Рекомендация:** передавать captured `*Config` через всю resolve/build
операцию. `workerSubAgentActive` должен принимать этот `cfg`; resolved result
должен нести provider config/options той же generation.

### P1-2. `RemoveProviderAPIKey` мутирует уже опубликованный snapshot без generation bump

После `RemoveConfigField` (который reload-ит и публикует snapshot) метод напрямую
делает `s.loadSnapshot().config.Providers.Del(providerID)`
(`internal/config/store.go:1413-1421`). `csync.Map` защищает память от data race,
но не сохраняет логическую immutability snapshot-а. Между publish и `Del`
другой goroutine может построить и закэшировать client; затем provider исчезнет
без новой generation и cache invalidation. Старый snapshot также меняется задним
числом.

**Рекомендация:** публиковать независимую копию `Config/Providers` с новой
generation, как уже делает исправленный `SetProviderRuntimeConfig`
(`internal/config/store.go:1035-1083`), либо включить удаление в единственный
reload transition.

### P1-3. Join interrupt ticker-а не ограничен собственным deadline

Предыдущий LIFO-defer deadlock исправлен: cancel и join теперь объединены в
правильном порядке (`internal/agent/coordinator.go:1093-1103`, `:2803-2811`).
Но join остаётся голым `<-tickerDone>`. Если уже начатый `handleInterruptTick`
зависнет в нижележащей операции, игнорирующей parent cancellation, весь
`Coordinator.Run` не вернётся. Это потенциальный путь того же симптома
«сессия замёрзла», хотя статически доказанного зависания конкретной dependency
в этом проходе не найдено.

**Рекомендация:** дать каждому tick ограниченный operation context и явно
включить ticker в общий lifecycle accounting. Не маскировать проблему простым
timeout join-а, оставляющим goroutine писать в закрываемую DB.

### P1-4. Outbox drain не выполняется при старте

Pump делает initial tick main queue, но orphan drain запускается только по
отдельному ticker (`internal/session/run_queue_pump.go:492-515`). Production
interval — 15 секунд. Короткоживущий процесс может стартовать и завершиться,
так и не попытавшись восстановить outbox. Это не заменяет P0-4, но ухудшает
liveness даже для корректных `pending` строк.

**Рекомендация:** выполнять initial orphan drain до/рядом с initial main tick,
а лучше убрать вторую независимую scheduling state machine после атомарного
переноса.

## Что действительно исправлено

Статическое чтение подтверждает полезность следующих изменений:

- stale checkpoint publish защищён generation check и `RowsAffected`; состояние
  checkpoint writer синхронизировано, запись ограничена 30-секундным context;
- FIFO pending inject получил стабильный tie-breaker;
- transient enqueue error orphan-outbox теперь возвращает row в `pending`;
- прежний defer-order deadlock interrupt ticker-а исправлен;
- `SetProviderRuntimeConfig` больше не меняет старый Providers map in-place и
  публикует новую generation;
- renewal/Ack/Nack ограничены текущим `leased_by`;
- raw prompt logging по умолчанию заменён на hash/length;
- мёртвые interrupt-recovery ветви и vacuous checkpoint tests удалены.

Эти исправления не следует откатывать. Проблема не в том, что «ничего не
починили», а в том, что локальные guards добавлены в систему с несколькими
независимыми путями ownership и acceptance, и новые комбинации остались без
единого инварианта.

## Общий обзор и запахи архитектуры

### 1. Слишком много state machines для одной принятой команды

Сейчас direct turn, mailbox submit/replacement, pending inject, durable queue и
orphan outbox имеют разные определения «accepted», ownership, retry и terminal
success. Булевый `FromDurableQueue` меняет смысл универсального `submit`, что и
породило P0-2. Дизайн из `docs/design/2026-08-12-state-machine-unification.md`
движется в правильную сторону: нужен один явный lifecycle с durable identity,
lease owner/generation и terminal state.

### 2. Критическая логика слишком концентрирована

На текущем HEAD:

- `internal/agent/agent.go` — 5053 строки;
- `internal/agent/coordinator.go` — 3649 строк;
- `internal/agent/mailbox.go` — 1396 строк;
- `internal/session/run_queue_pump.go` — 1256 строк;
- `internal/config/store.go` — 1819 строк.

Большие комментарии полезны как история расследования, но часто заменяют
машинно проверяемый invariant и уже противоречат коду (пример — live
replacement interrupt-а). Следует выделить ownership transition API, durable
execution API и immutable config resolver в меньшие компоненты.

### 3. API допускают опасное неправильное использование

- `mb.submit` возвращает ownership result, но вызывающий код может его
  проигнорировать; при durable flag операция превращается в no-op.
- `Config()`/`Snapshot()` возвращают mutable pointer, а immutability держится
  только на дисциплине вызывающих методов.
- outbox claim не несёт lease/fencing metadata.
- success `Coordinator.Run == nil` недостаточен как критерий `Ack`, если внутри
  могли породиться обязательные continuation-и.

Нужны типизированные операции вместо флагов: `AcceptExternal`,
`ContinueExecution`, `InterruptOwnedExecution`, `CommitExecution`.

### 4. Coverage проверяет детали, но пропускает сквозные инварианты

Новый код содержит много unit/fault tests, однако один тест прямо фиксирует
вечный `processing`, а durable-compaction дефект был описан в плане и всё равно
остался. Release gate должен включать state-machine сценарии через реальные
границы компонентов:

- accept -> cancel -> live handoff -> commit;
- durable lease -> compaction -> continuation -> one Ack;
- renew latency > margin -> cancel before DB expiry;
- crash after outbox claim -> recovery by a new pump;
- config reload в каждой seam между resolve/build/prompt/provider options.

## Рекомендуемый порядок исправлений

1. Закрыть P0-2 compaction continuation: это прямой silent data loss на
   существующем durable path.
2. Исправить watchdog на абсолютный DB lease deadline и добавить delayed-success
   renewal regression test.
3. Сделать перенос orphan outbox атомарным и добавить crash recovery test.
4. Решить P0-1 live handoff для `crush run`, не возвращая double ownership.
5. Провести config snapshot через всю одну resolve/build операцию и убрать
   in-place mutation из `RemoveProviderAPIKey`.
6. Ограничить lifecycle interrupt ticker-а и сделать initial outbox drain.
7. После point fixes вернуться к единой state machine; иначе следующий guard
   снова с высокой вероятностью закроет один путь и сломает соседний.

## Release gate после исправлений

Минимальный критерий GO:

- все четыре P0 сценария закрыты кодом и сквозными regression tests;
- нет принятой команды без ровно одного текущего owner/recovery owner;
- `Ack` возможен только после полного logical execution, включая continuation;
- watchdog использует тот же абсолютный expiry, который видит БД;
- `processing` outbox восстанавливается после смерти процесса;
- config generation не смешивается внутри одного call build;
- затем отдельно пройдены обычные test/lint/race проверки (они намеренно не
  выполнялись в рамках этого read-only статического аудита).

