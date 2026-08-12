# Повторное release-readiness ревью после исправлений multi-agent/session freeze

Дата: 2026-08-12  
Режим: только статическое чтение production-кода и истории Git; тесты, сборка, lint и race detector не запускались.  
Точка ревью: `3a145e60b3a0b790456667d7fd9eafe58f8d6fad` (`main`).  
База сравнения: `684e79fb8e8de5ab317537f0c7b8d51e2e1fcdce` — точка предыдущего release-readiness review.  
Диапазон: `684e79fb..3a145e60`, 28 коммитов, 35 файлов, `+3514/-137`.  
Пользовательские изменения `web/dist/.gitkeep` и `dev/` в оценку не входили и не изменялись.

## Итог

**Вердикт: NO-GO для стабильного релиза. Не всё исправлено.**

Последний раунд действительно закрыл несколько важных дефектов: `LogicalCallID` теперь переживает durable round-trip, consume interrupt и enqueue объединены одной транзакцией, ожидание main loop у pump ограничено, terminal message защищён от перезаписи старым partial checkpoint, CLI args редактируются, а production callers больше не могут молча построить call с `pinned=nil`.

Но поверх этих исправлений появились или остались три release blocker:

1. Атомарный cross-process interrupt после durable enqueue кладёт **вторую копию того же call** в live mailbox. Live owner выполняет её, а durable row затем выполняется pump ещё раз.
2. Поздний checkpoint больше не портит terminal row в SQLite, но всё равно публикует stale partial message подписчикам. UI может снова показать уже завершённую сессию как незавершённую/«зависшую». Одновременно checkpoint fencing содержит реальные data race.
3. Если durable enqueue orphaned call не состоялся, fallback не даёт durability guarantee: он запускает call лишь in-process на 30 секунд. Длинный provider/tool turn, shutdown или crash теряют уже принятую работу.

Я не нашёл нового очевидного цикла вида `mutex A -> mutex B -> mutex A` в основном mailbox path. Однако это не означает отсутствия freeze: оставшиеся сценарии основаны на двойном выполнении, stale UI state, внешнем I/O без полноценного fencing и фоновых goroutine, не вошедших в lifecycle join.

## Статус замечаний предыдущего отчёта

| Предыдущее замечание | Статус на `3a145e60` |
|---|---|
| P0-1: `LogicalCallID` теряется на durable boundary | **Закрыто.** ID добавлен в DTO, сериализацию и rebuild; durable-origin orphan больше не создаёт вторую row. |
| P0-2: consume cross-process interrupt происходит до гарантированного handoff | **Исходное окно потери закрыто, но создан новый P0 duplicate-path.** Транзакция надёжно создаёт durable row, после чего тот же call отдельно записывается в mailbox. |
| P0-3: orphan enqueue failure оставляет runnerless mailbox | **Частично.** Runner теперь есть, но он best-effort, ограничен 30 секундами и не имеет durable record. |
| P0-4: late checkpoint может затереть terminal message в БД | **DB-часть закрыта.** `WHERE finished_at IS NULL` сохраняет terminal row. Event/UI и синхронизация generation всё ещё сломаны. |
| P1-1: `RunQueuePump.Stop` бесконечно ждёт main loop | **Закрыто.** Workers и main loop используют единый пятисекундный deadline, forced state передаётся в `App.Shutdown`. |
| P1-2: execution продолжается после ошибок lease renewal | **Частично.** Fail-closed проверка добавлена, но она выполняется той же goroutine после потенциально 30-секундного DB call при TTL 30 секунд. |
| P1-3: model cache переживает config/credential changes | **Частично.** Generation и явный reset добавлены, но snapshot читается рвано, а немедленный 401 retry повторяет старый pinned call. |
| P1-4: interrupt paths откатываются к shared/global model | **Закрыто для проверенных production callers.** `resolveSessionModels` обязателен, `buildCall`/`runInternal` отвергают nil snapshot. |
| SEC-1: CLI provider логирует prompt через raw argv | **Закрыто в CLI provider.** Но новый orphan fallback снова пишет пользовательский prompt целиком на ERROR. |
| P2-1: cleanup lock metadata имеет compare/delete TOCTOU | **Явно принят как diagnostic risk.** Это не найденная причина deadlock, но риск ложной диагностики остаётся. |

## Release blockers

### P0-1. Один cross-process interrupt имеет одновременно durable- и mailbox-владельца

`handleInterruptTick` сначала атомарно удаляет `pending_injects` row и создаёт `session_run_queue` row (`internal/agent/coordinator.go:2377-2389`). Это правильное устранение старого окна потери.

Сразу после commit тот же `call` передаётся в `InterruptAndReplace` (`internal/agent/coordinator.go:2391-2401`). Построенный здесь call не имеет `FromDurableQueue=true`: этот флаг выставляется только позднее, когда pump делает `RebuildSessionAgentCall` (`internal/agent/coordinator.go:2720-2746`). `mailbox.interruptAndReplace` безусловно сохраняет call в `mb.replacement` (`internal/agent/mailbox.go:771-815`).

Это ровно тот double-execution hazard, который уже описан в guard для `mailbox.submit`: live owner исполняет in-memory copy, затем pump исполняет оставшуюся durable row (`internal/agent/mailbox.go:371-381`). Guard здесь не помогает, потому что interrupt идёт через `interruptAndReplace`, а не через `submit`, и live copy никак не Ack-ает durable row.

Достижимая последовательность:

1. Turn A владеет mailbox и OS lock.
2. Ticker атомарно переносит interrupt B в durable queue.
3. `InterruptAndReplace` одновременно сохраняет B как replacement и отменяет A.
4. Живой dispatcher извлекает replacement и выполняет B.
5. Durable row B остаётся `pending`/возвращается в `pending`, пока сессия занята.
6. После освобождения сессии pump арендует row B и выполняет тот же provider/tool turn повторно.

Риск — не только дублированный текст: повторно запускаются неидемпотентные tools и внешние side effects.

Текущие tests закрепляют ошибочное состояние: `TestHandleInterruptTick` требует mailbox replacement (`internal/agent/coordinator_test.go:1636-1655`), а fault-injection test отдельно требует durable row (`internal/agent/p0_2_fault_injection_test.go:86-100`). Ни один тест не запускает оба механизма до конца и не считает provider/tool executions.

Что исправить:

- выбрать единственного владельца после commit;
- наиболее простой контракт: durable enqueue -> отмена текущей generation/dispatcher -> wake pump, **без** `mb.replacement`;
- альтернативно live path должен lease/исполнять/ack-ать именно ту же durable row с owner/generation fencing; простого `FromDurableQueue=true` недостаточно, потому что `interruptAndReplace` этот флаг не учитывает;
- добавить end-to-end regression с реальным mailbox и pump: один inject, одна queue row, ровно один provider/tool execution, row Ack/delete после выполнения.

### P0-2. Late checkpoint защищает SQLite, но может снова «заморозить» UI

Partial checkpoint вызывает условный `UpdateMessageIfNotTerminal` (`internal/message/message.go:273-297`). SQL корректно не меняет terminal row (`internal/db/sql/messages.sql:48-60`). Но query объявлен как `:exec`, поэтому generated method возвращает только error, а не число изменённых строк (`internal/db/messages.sql.go:586-609`).

После nil error `message.Service.Update` **всегда** публикует переданный partial snapshot (`internal/message/message.go:298-325`) — даже если SQL обновил 0 rows, потому что terminal finish уже победил.

Следствие:

1. Terminal message успешно записан и опубликован.
2. Старый checkpoint DB call возвращается позже.
3. `WHERE finished_at IS NULL` отклоняет stale DB update.
4. Service не видит `0 rows affected` и публикует stale `Finish.Partial=true`.
5. Последним событием в web/TUI становится незавершённая копия; без следующего события UI может выглядеть зависшим до reload.

Отдельно generation fence сам содержит гонки:

- `checkpointGeneration++` выполняется без `sessionLock` (`internal/agent/agent.go:2187-2205`);
- старая goroutine читает generation под `sessionLock` в одном месте и без lock в другом (`internal/agent/agent.go:2221-2230`, `2258-2263`);
- `checkpointPartsLen` читается под lock, но записывается вне lock (`internal/agent/agent.go:2221-2243`, `2258-2263`);
- после пятисекундного timeout `stopCheckpoint` разрешает следующему writer стартовать, пока старый всё ещё жив (`internal/agent/agent.go:2270-2298`).

Таким образом комментарий о fencing не соответствует memory model Go. `runWg` корректно заставит shutdown перейти в forced mode, но не устраняет race и stale publish.

Что исправить:

- сменить SQL query на `:execrows`/`RETURNING` и не публиковать partial event при `rowsAffected == 0`;
- синхронизировать generation и `checkpointPartsLen` одним mutex либо atomic protocol;
- держать per-writer coalescing state локально, а не совместно между overlapping generations;
- дать DB write не только cancel, но и собственный deadline;
- добавить regression на порядок `terminal publish -> late conditional checkpoint`, проверяющий и DB, и последний broker event; отдельно прогнать race detector для overlap старой и новой generation.

### P0-3. Orphan fallback всё ещё теряет принятую работу

При ошибке durable enqueue `restartOrphanedWithRetry` вызывает `startBoundedDetachedRun` (`internal/agent/agent.go:1235-1246`). Это лучше старого runnerless mailbox: попытка выполнения действительно начинается.

Но runner использует только process memory и `context.WithTimeout(..., 30*time.Second)` (`internal/agent/agent.go:1266-1315`). Комментарий рядом честно признаёт, что crash, kill или повторная DB-проблема теряют call без durable trace (`internal/agent/agent.go:1275-1286`). Дополнительно нормальный LLM/tool turn может законно длиться больше 30 секунд и будет отменён самим fallback.

Это не соответствует более сильному комментарию выше, что fallback «guarantees the call executes» (`internal/agent/agent.go:1240-1243`), и не закрывает прежний release gate: принятый call должен либо иметь durable recovery record, либо явный terminal outcome.

Что исправить:

- не признавать ownership handoff завершённым без durable marker;
- использовать retryable outbox/локальный WAL, записываемый той же транзакцией, что и принятие intent;
- если persistence полностью недоступен, сохранять call в наблюдаемом terminal-failure состоянии и возвращать ошибку вызывающему уровню, а не маскировать потерю фоновой попыткой;
- timeout выполнения не должен быть равен timeout enqueue; provider/tool lifecycle должен подчиняться обычным run limits.

## P1 — серьёзные проблемы до stable release

### P1-1. Lease fail-closed срабатывает после возможного истечения lease

Production TTL равен 30 секундам; renewal идёт раз в `TTL/3`. Но каждый `RenewRunQueueLease` получает отдельный DB context до 30 секунд (`internal/session/run_queue_pump.go:707-725`, `849-855`). Проверка `time.Since(lastSuccessfulRenewal) >= TTL` выполняется той же goroutine **до** DB call и повторится лишь после его возврата (`internal/session/run_queue_pump.go:822-846`).

Если первый renewal начался примерно на 10-й секунде и завис до своего 30-секундного timeout, текущий executor продолжит реальную работу примерно до 40-й секунды, хотя lease истёк на 30-й. Другой pump уже может выполнять ту же row. Если driver игнорирует cancellation, окно ещё больше. Комментарий о bounded-to-one-TTL residual window (`internal/session/run_queue_pump.go:783-801`) слишком оптимистичен.

Нужен независимый watchdog timer, который отменяет `execCtx` до expiry с safety margin, а timeout отдельного renewal должен быть меньше оставшегося safe lease budget. Для строгой защиты неидемпотентных writes всё равно нужен fencing token.

### P1-2. 401 retry повторяет старый pinned provider client

`runInternal` один раз строит `agentCall` с `LargeModel` из resolved snapshot, затем closure `run` всегда передаёт тот же call в `currentAgent.Run` (`internal/agent/coordinator.go:968-1017`).

После 401 `retryAfterUnauthorized` обновляет credentials и вызывает `UpdateModels`, который очищает cache и перестраивает shared agent (`internal/agent/coordinator.go:3158-3235`). Но `runWithUnauthorizedRetry` просто вызывает ту же closure второй раз (`internal/agent/coordinator.go:3167-3174`). Pinned `agentCall.LargeModel` остаётся старым client/token, поэтому немедленный retry может предсказуемо получить второй 401.

Тест `TestUpdateModels_ClearsCacheOnCredentialRefresh` проверяет только прямой вызов `clearModelCache`, а не реальный `401 -> refresh -> rebuild call -> retry` (`internal/agent/coordinator_cache_invalidation_test.go:103-134`).

После refresh нужно заново вызвать `resolveSessionModels`, пересчитать provider options и построить новый call, сохранив logical request ID. Нужен test с двумя provider clients: старый всегда 401, новый после rotation успешен; retry обязан вызвать именно новый.

### P1-3. Cache key имеет generation, но config snapshot читается неатомарно

`resolveSessionModels` отдельно читает large config, small config и затем generation (`internal/agent/coordinator.go:597-600`, `642-666`). `buildModelsFromCfg` ещё раз независимо читает provider configs (`internal/agent/coordinator.go:1883-1899`), а prompt prefix берётся очередным `Config()` (`internal/agent/coordinator.go:697-704`). Reload между этими чтениями позволяет собрать смешанную пару generation N/N+1 и сохранить её под ключом более новой generation.

Кроме того, `ConfigStore.Generation` обещает increment на каждую mutation (`internal/config/store.go:210-214`), но `SetProviderRuntimeConfig` меняет Providers map текущего snapshot in-place и generation не увеличивает (`internal/config/store.go:1025-1041`). Явный `clearModelCache` в текущем credential path частично маскирует это, но контракт cache key остаётся неверным.

Нужен единый API `Snapshot() (config, generation)`, захватывающий оба значения из одного `storeSnapshot`; этот snapshot следует передавать через model/provider/prompt build без повторных `Config()`.

### P1-4. Interrupt ticker не соответствует lifecycle turn и не join-ится

`startInterruptTicker` запускает fire-and-forget goroutine и завершает её после первого interrupt (`internal/agent/coordinator.go:2277-2306`). Комментарий утверждает, что replacement turn получит fresh ticker. В действительности ticker создаётся вокруг одного внешнего `currentAgent.Run`, а `sessionAgent.Run` сам обрабатывает replacement turns внутренним loop. После первого interrupt live replacement может выполняться долго уже без ticker; второй cross-process interrupt останется pending и не прервёт его.

Goroutine также не имеет `done`/WaitGroup join. Отмена context при возврате outer Run не доказывает, что уже начавшийся `handleInterruptTick` завершился до `CancelAll`/DB cleanup.

Ticker нужно либо привязать к каждой реальной generation, либо сделать единым session-level watcher на всё время ownership. Его остановка должна быть joinable и входить в shutdown accounting.

## Security

### SEC-1. Новый orphan fallback логирует prompt целиком на ERROR

CLI provider теперь корректно редактирует `-p/--prompt` и показывает raw prompt только в opt-in diagnostic mode. Но `startBoundedDetachedRun` пишет `call.Prompt` целиком и при старте, и при ошибке (`internal/agent/agent.go:1291-1315`). Пользовательский текст может содержать credentials, исходный код и другие секреты; ERROR logs обычно хранятся дольше и собираются централизованно.

Оставить только session/logical IDs, длину и безопасный hash. Raw prompt допустим только под тем же явно включаемым diagnostic guard.

## P2 и code smell

### P2-1. FIFO pending injects неоднозначен в пределах одной секунды

`created_at` хранится в секундах, но `PeekInterruptInject` использует только `ORDER BY created_at ASC` (`internal/session/session.go:1208-1225`). `DrainPendingInjects` имеет ту же проблему (`internal/session/session.go:1123-1126`). Несколько быстрых inject могут быть обработаны в неопределённом порядке. Добавить `rowid ASC` как tie-breaker, аналогично уже исправленным message/run-queue queries.

### P2-2. В production остались obsolete interrupt recovery paths

После атомарного rewrite `requeueInterruptMessage` больше не вызывается production-кодом, только тестом. `recreatePendingInjectRowPostAccept` также существует только ради тестов. Вместе с ними остаются `DeleteInterruptInject` и старый delete/recreate protocol, хотя реальный handler использует новую транзакцию.

Это опасный smell именно в concurrency code: тесты продолжают поддерживать второй, уже неактуальный жизненный цикл и создают ложное ощущение покрытия. Удалить dead helpers/API/tests или явно вынести legacy path, если он действительно ещё нужен.

### P2-3. Комментарии часто описывают историю, а не текущий контракт

Примеры:

- `interruptAndReplace` называет in-memory pointer «durably recorded» (`internal/agent/mailbox.go:771-815`);
- orphan fallback одновременно «guarantees execution» и «best-effort, can be lost» (`internal/agent/agent.go:1240-1243`, `1275-1286`);
- SQL comment обещает row count, но query имеет `:exec` (`internal/db/sql/messages.sql:48-60`);
- interrupt ticker обещает fresh ticker на replacement turn, которого фактически нет (`internal/agent/coordinator.go:2277-2285`).

Такие расхождения уже коррелируют с найденными дефектами. Для concurrency-critical функций полезнее коротко фиксировать owner, durable record, cancellation authority и terminal transition, а историю вынести в design doc.

### P2-4. Critical state machine размазан по слишком крупным файлам

На текущем HEAD: `agent.go` — 5046 строк, `coordinator.go` — 3589, `mailbox.go` — 1380, `run_queue_pump.go` — 1015, `session.go` — 1736. Один logical call меняет состояние в coordinator, mailbox, agent, session service, SQL, app adapter и broker. Именно поэтому локальное исправление consume/enqueue не заметило второго владельца в mailbox.

Нужна одна формальная state machine, например:

`accepted -> durable(rowID) -> leased(owner,generation) -> running -> committed | retryable | terminal_failed`

Mailbox должен отвечать за ordering/wakeup, durable store — за ownership/retry, message service — за versioned/fenced writes. Сейчас эти обязанности перекрываются.

## Что выглядит хорошо

- Durable DTO теперь действительно сохраняет `LogicalCallID`, а rebuild восстанавливает его.
- `FromDurableQueue` guard в обычном `mailbox.submit` правильно исключает in-memory duplicate для pump-origin call.
- Транзакция `ConsumeInterruptInjectAndEnqueue` корректно связывает exact `injectID` с enqueue и rollback-ит обе операции вместе.
- `RunQueuePump.Stop` теперь ограничивает и workers, и main loop единым deadline; `App.Shutdown` не закрывает DB под известными live writers при forced shutdown.
- `pinned=nil` больше не даёт скрытого shared-model fallback в проверенных production paths.
- CLI provider по умолчанию больше не пишет prompt/raw argv в INFO.
- Код всё чаще документирует at-least-once и forced-shutdown ограничения честно; это правильное направление, хотя несколько комментариев ещё противоречат реализации.

## Рекомендуемый порядок исправлений

1. Удалить двойное владение cross-process interrupt: после atomic enqueue оставить только durable row и cancellation/wakeup либо ввести lease/ack одного row для live path.
2. Сделать partial checkpoint update observable (`rowsAffected`) и запретить stale publish; синхронизировать generation/coalescing state.
3. Определить честный durable contract orphan handoff без 30-секундной best-effort потери.
4. Отделить lease-expiry watchdog от blocking renewal call и ввести safety margin/fencing.
5. После credential refresh перестраивать pinned call перед retry; добавить end-to-end 401 rotation test.
6. Ввести атомарный config snapshot и использовать его на всём model build path.
7. Сделать interrupt watcher per-ownership/per-generation и joinable; проверить два быстрых interrupt подряд.
8. Удалить raw prompt из ERROR logs, добавить FIFO tie-breaker и убрать dead recovery paths.
9. После фиксов провести stabilization freeze и один составной release gate, а не набор helper tests.

## Минимальный release gate

Перед stable release одновременно должны выполняться свойства:

1. Один cross-process interrupt создаёт один durable execution identity и не более одного provider/tool execution, включая busy-owner case.
2. Два interrupt подряд прерывают две последовательные active generations в правильном FIFO-порядке.
3. Late checkpoint после terminal finish не меняет ни SQLite row, ни последнее UI event; overlap generations не даёт race detector finding.
4. Ни один accepted orphan call не существует только в process memory; crash/shutdown оставляет retryable либо terminal durable state.
5. Renewal, зависший дольше safety budget, отменяет executor **до** lease expiry; stale owner не может persist-ить результат без fencing.
6. `401 -> credential rotation -> retry` использует новый provider client и не создаёт второй logical request.
7. Concurrent config reload не может смешать models/providers/prompt prefix разных generations и записать результат под новый cache key.
8. `CancelAll`/`Shutdown` join-ит interrupt/checkpoint/pump writers либо явно возвращает forced state, не закрывая их ресурсы.
9. Production INFO/ERROR logs не содержат raw prompt, history, attachment content или auth material.

До выполнения этих условий `3a145e60` не следует маркировать как стабильный релиз.
