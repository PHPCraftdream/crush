# Релизный аудит конкурентности: зависания после внедрения мультиагентности

Дата: 2026-08-07  
Ветка: `main`  
HEAD: `c33ce5a8fd00dd37fc371710513d1f8565fe4d75`  
База предыдущего release-review: `0fdfefa04e734ba083cb5dadfe3e6cc2cd9fac74`  
Проверенный новый диапазон: `0fdfefa04e734ba083cb5dadfe3e6cc2cd9fac74..c33ce5a8fd00dd37fc371710513d1f8565fe4d75`

В новом диапазоне 54 коммита и 77 изменённых файлов (11 589 добавлений,
1 434 удаления). Более широкий двухнедельный диапазон с 2026-07-24 содержит
304 коммита.

## Вердикт

**NO-GO для стабильного релиза.**

Основная mailbox-миграция стала существенно надёжнее, чем 2026-08-04:
структурно исправлены ранее найденные дефекты late interrupt, idle interrupt,
shutdown restart, stale cancel, title join, lock cleanup и ownership
компакции. Классического цикла захвата Go mutex в текущем mailbox-коде я не
обнаружил.

Но более сильный релизный инвариант — «у каждого принятого call всегда есть
живой или durable runner» — всё ещё не выполняется. Есть как минимум два
детерминированных пути, где call принимается, но остаётся в памяти без
goroutine, обязанной его выполнить. Для пользователя это выглядит как
зависшая сессия, хотя mailbox уже может показывать `mbIdle`. Также остались
неограниченные I/O/shutdown-ожидания и гонка изменения transcript в rerun.

Это статический review в режиме только чтения. По прямому указанию оператора
не запускались тесты, сборка, линтеры, race detector и исполняемые команды
Crush. Единственное изменение workspace — этот отчёт.

## Релизные блокеры

### P0-1. Обычная ошибка owner может оставить уже принятые calls без runner

**Доказательства:**

- Конкурентный `Run`, пришедший во время ownership сессии, добавляет call в
  очередь и сразу возвращает `(nil, nil)` (`internal/agent/agent.go:1075-1082`,
  `internal/agent/mailbox.go:332-351`). Его собственная goroutine закончилась;
  выполнение теперь полностью зависит от текущего owner.
- Не-cancellation ошибки preamble возвращают `hasNext=false`
  (`internal/agent/agent.go:1481-1499,1501-1510,1530-1540`).
- Provider/DB/finalization error также возвращается без обычного final drain,
  если это не специальная ветка cancellation/replacement
  (`internal/agent/agent.go:2775-2787`). Ошибка inline full summary делает то
  же самое (`internal/agent/agent.go:2790-2805`).
- После этого `Run` выходит через cleanup defer. `abandonOwnership` намеренно
  оставляет работу в `mb.submitted`, переводит mailbox в `mbIdle` и ничего не
  запускает (`internal/agent/agent.go:1119-1126`,
  `internal/agent/mailbox.go:690-736`). Сам лог говорит, что работа ждёт
  «следующего `Run()`».

**Проигрывающая последовательность:**

1. Turn A владеет сессией.
2. Приходит turn B, добавляется в `submitted`, а его caller получает success.
3. A получает provider 5xx, preamble timeout/DB error, summary error или другую
   не-cancel ошибку до final drain.
4. A завершается. B остаётся в idle mailbox без runner.
5. B выполнится только если какое-то несвязанное будущее действие снова
   вызовет `Run`. Причём новый call выполнится раньше B, поэтому повторные
   ошибки могут ещё и бесконечно откладывать и переупорядочивать старую работу.

Текущие mailbox-тесты доказывают, что запись *сохраняется* для будущего owner
(`internal/agent/mailbox_migration_test.go:51-82`); они не доказывают liveness,
если будущий owner не придёт. Сохранение — не выполнение.

**Что исправить:** каждый выход из ownership, включая все error paths, должен
выполнить одну атомарную handoff-операцию: оставить живой dispatcher, запустить
новый runner либо сохранить calls для durable worker. `abandonOwnership` не
может возвращать `hadWork=true`, никому не назначив ответственность за работу.

### P0-2. Detached/restarted call может проиграть гонку за межпроцессный lock и исчезнуть

**Доказательства:**

- Idle-ветка `InterruptAndSend` сообщает success после запуска fire-and-forget
  goroutine (`internal/agent/coordinator.go:2162-2172,2199-2206`).
- Calls, осиротевшие во время `mbReleasing`, запускаются тем же одноразовым
  detached-механизмом (`internal/agent/agent.go:918-969`).
- `Run` сначала становится внутрипроцессным mailbox owner и только затем
  пытается взять OS session lock (`internal/agent/agent.go:1075-1082,
  1173-1199`). Если другой процесс выигрывает lock, `Run` возвращает error.
- Cleanup defer затем попадает в P0-1: переводит mailbox в idle и оставляет
  submitted work для гипотетического будущего `Run`. Сам detached call не был
  сохранён durable и повторно не запускается.

Особенно прямой эта гонка является для `restartOrphaned`: старый lock только
что отпущен, поэтому другой процесс законно может выиграть до reacquire в
detached goroutine. Если запущено несколько orphaned calls, один из них может
взять локальный mailbox, остальные встанут за ним; проигрыш внешнего OS lock
этим owner оставит без runner всю локальную очередь.

Cross-process путь `sessions inject --interrupt` восстанавливается ещё хуже:
`ConsumeInterruptInject` удаляет durable pending row внутри транзакции до того,
как `requeueInterruptMessage` запускает detached run
(`internal/session/session.go:1110-1146`,
`internal/agent/coordinator.go:2093-2137`). Если detached run проиграет OS-lock
гонку, user message останется в истории, но сигнал на выполнение уже потерян.

**Что исправить:** не подтверждать detached work, пока она не записана durable
или не получен OS lock. Как минимум `SessionLockBusyError` требует безопасного
для ownership retry/requeue с bounded backoff и durable source record. Надёжная
архитектура — persisted per-session run queue с claim/lease/ack; goroutine плюс
log line не дают гарантии доставки.

## Риски высокого приоритета

### P1-1. У manual compaction есть отдельная runnerless-очередь на error path

Manual compaction получает mailbox ownership. Пришедший в это время `Run`
нормально встаёт в очередь. Но обе ошибки acquisition OS lock сразу вызывают
`abandonOwnership` и возвращаются (`internal/agent/agent.go:2920-2937`), а
ошибка summary body возвращается до `popFirstSubmitted`
(`internal/agent/agent.go:2952,2981-2994`). Первый queued call запускается
только после успешной компакции.

Это то же нарушение liveness, что P0-1, но в отдельной реализации owner.
Compaction finalizer обязан drain/restart pending calls при любом результате,
а не только при success.

### P1-2. Manual compaction может навсегда остаться busy в release session lock

Обычный turn представляет teardown lock состоянием `mbReleasing`, отпускает
`mailbox.mu` и оставляет control plane отзывчивым. Manual compaction не
использует эту state machine: mailbox остаётся `mbOwned` во время прямого
вызова `lk.Release()` (`internal/agent/agent.go:2952-2981`).

`SessionLock.Release` выполняет filesystem calls без context/timeout: truncate,
seek, sync, удаление sidecar, OS unlock и close
(`internal/session/lock.go:263-283,320-330`). На зависшем filesystem/AV/SMB
сессия останется owned навсегда. `Cancel` не может прервать эти вызовы.

Нужно использовать ту же release state machine, что и в обычном turn, а
диагностическую очистку metadata по возможности убрать с correctness-critical
пути unlock. Go timeout вокруг синхронного filesystem call не останавливает сам
вызов; ownership-дизайн должен переносить брошенную cleanup goroutine, не
объявляя OS lock свободным раньше времени.

### P1-3. Per-turn snapshot модели не покрывает summarization

Коммит `a19c8d0c` правильно ввёл immutable `turnConfig` для основного turn
(`internal/agent/agent.go:335-373,1433-1444`). Оба summary-пути обходят его и
заново читают общий mutable agent state:

- `runSummarizeBody`: `internal/agent/agent.go:3024-3027`;
- `runSummarizeSilent`: `internal/agent/agent.go:3197-3199`.

Поэтому inline compaction может начаться внутри pinned turn сессии A, но взять
позднее записанную session B модель/prompt prefix, продолжая использовать
provider options, вычисленные для A (`internal/agent/agent.go:2790-2805`).
Manual summary также отдельно читает `currentAgent.Model()` для выбора provider
и options (`internal/agent/coordinator.go:2393-2408`), а concurrent overrides
продолжают менять shared agent (`internal/agent/coordinator.go:531-584`).

Результат — неверные provider options, accounting, authentication error или
визуально зависшая компакция. Нужно передавать один immutable snapshot модели,
prefix и provider options в обе summary-функции. Manual summary должен
разрешать его из target session, а не из process-global last-writer-wins state.

### P1-4. Cleanup ошибки summary использует уже мёртвый context

Когда stream возвращает `context.Canceled`, visible summary вызывает
`messages.Delete(ctx, ...)` с отменённым context
(`internal/agent/agent.go:3109-3113`). Silent summary делает то же самое и
игнорирует ошибку (`internal/agent/agent.go:3286-3290`). При deadline visible
ветка пытается вызвать `messages.Update` с истёкшим context
(`internal/agent/agent.go:3115-3121`).

Обычный результат — незавершённая summary row в DB; UI может продолжать
показывать её in-flight до startup recovery после перезапуска. Для
cancellation/error finalization нужен короткий bounded
`context.WithTimeout(context.WithoutCancel(ctx), ...)`, как уже сделано в
последующей commit phase.

### P1-5. Заявленные пять секунд CancelAll не ограничивают весь shutdown

`CancelAll` запрещает новые turns, но через пять секунд возвращается даже при
живых owners (`internal/agent/agent.go:4010-4057`). Затем `App.Shutdown`
начинает cleanup ресурсов вопреки своему комментарию о необходимости дождаться
agents и делает безусловный `wg.Wait()` (`internal/app/app.go:1777-1819`).

DB cleanup игнорирует shutdown context (`internal/app/app.go:161-183`).
`db.Release` держит глобальный `poolMu` во время close SQL pools
(`internal/db/connect.go:192-225`); `sql.DB.Close` может ждать active
operations. Поэтому agent/provider/tool, не завершившийся за пять секунд,
может гоняться с DB close или заставить `App.Shutdown` ждать бесконечно.
Пятисекундный context помогает только cleanup functions, которые его реально
читают; он не ограничивает `wg.Wait`.

Нужно учитывать live dispatchers настоящим join primitive, разделить «перестать
принимать» и «все owners завершились» и явно выбрать forced-shutdown policy.
Нельзя начинать DB close, пока writers ещё могут работать. Если после grace
period cleanup разрешено зависнуть, внешний shutdown wait тоже обязан иметь
реальный путь выхода.

### P1-6. Rerun меняет transcript, даже если cancellation не завершился

Web rerun handler ждёт idle максимум десять секунд, но не проверяет, что idle
действительно наступил. После timeout он всё равно удаляет tail и исходное user
message, затем запускает/ставит в очередь новый run, хотя старый owner может
быть активен (`internal/server/handlers.go:2024-2076`).

Provider/tool законно может реагировать на cancellation дольше десяти секунд.
Тогда старый turn продолжит писать в history одновременно с её удалением и
пересозданием rerun. Это гонка повреждения transcript, а не просто UI delay.

После deadline нужно fail closed с ответом «session still stopping» либо
выполнять весь rerun как ownership-serialized mailbox operation только после
фактического release текущим owner.

## Средние и архитектурные риски

### P2-1. Liveness принятых очередей зависит от внешних callers

`QueueMessage` может добавить call в idle mailbox, не запустив runner
(`internal/agent/agent.go:3964-3966`,
`internal/agent/mailbox.go:1109-1119`). Сейчас production caller отсутствует,
поэтому это API footgun, а не непосредственно достижимый release blocker.
Метод следует удалить либо заменить атомарным submit-and-run/handoff API.

У manual summarize queue похожий внешний контракт liveness. Agent только
записывает `summarizeQueue`; забрать её и вызвать `Summarize` обязан tail
конкретного web `handleSendMessage`
(`internal/agent/agent.go:2866-2882,2997-3008`,
`internal/server/handlers.go:300-308`). Сессия, owned через другой entry point,
может навсегда оставить состояние «summarize queued». Queue draining должен
принадлежать session owner/finalizer, а не одному UI handler.

### P2-2. Web busy events не являются authoritative при concurrent sends

Второй `Run` для owned session сразу возвращает `(nil, nil)` после queueing.
Его `handleSendMessage` затем отправляет `busy=false`
(`internal/server/handlers.go:286-307`), хотя исходный owner ещё жив, а новый
call не выполнен. Mailbox остаётся authoritative, но clients могут показывать
неверное состояние и разрешать опасные follow-up actions. Busy state должен
исходить из ownership transitions, а не lifetime каждого request handler.

### P2-3. Cancellation не является hard execution boundary для каждого provider

State machine stream watchdog сейчас выглядит структурно корректно и различает
tool, idle и hard-cap причины. Но его force action всё равно только `cancel()`
(`internal/agent/stream_watchdog.go:265-438`). Если реализация provider
`Stream` игнорирует cancellation context, основной синхронный call остаётся
заблокирован. У CLI providers и обычных HTTP transports есть дополнительные
process/transport cancellation механизмы, но абстракция `fantasy` не доказывает
это свойство для каждого provider.

Manual compaction имеет десятиминутный context, но не запускает отдельный
watchdog; её `notifyWatchdog` работает лишь при inline compaction внутри turn,
который установил callback в context
(`internal/agent/agent.go:2866-2875,3024-3108`). Молчащий manual-summary
provider поэтому выглядит зависшим до десяти минут, а при игнорировании cancel
— дольше.

Для стабильной гарантии «never freeze» conformance provider к cancellation
должен стать явным контрактом и regression suite, с transport/process-specific
hard abort там, где это возможно.

## Что последние исправления действительно закрыли

1. `db546653` заставляет обычный final drain рассматривать late interrupt
   replacement до submitted work и release
   (`internal/agent/mailbox.go:496-562`).
2. `1410b398` запускает run для same-process idle interrupt вместо немедленной
   runnerless queue. P0-2 выше — остаток cross-process/error path, а не возврат
   исходного idle-дефекта.
3. `54fac263` latch-ит global/per-mailbox shutdown и отменяет generation вместе
   с dispatcher, поэтому shutdown больше намеренно не запускает turn N+1.
4. `d76b5eba` заменяет unbounded join title goroutine на bounded grace period.
5. `3b9549ae` и `5b2f3ff0` отделяют per-turn cancellation от dispatcher
   cancellation и восстанавливают replacement в preamble/inter-turn окнах.
6. `fc7f0284`, `d6bb2f87`, `d4f19852` и `e70ad793` помещают compaction и обычные
   runs под mailbox/OS-lock ownership и убирают filesystem release из-под
   `mailbox.mu` на обычном turn path.
7. `b94ace8c` больше не unlink-ит живой lock inode. Диагностика lock теперь
   считает реальный OS lock источником истины, а не только свежесть heartbeat.
8. Permission requests используют per-request buffered channels и больше не
   держат глобальный mutex в ожидании человека. Наследование auto-approval
   закрывает наблюдавшийся first-tool hang sub-agent.
9. Pub/sub publish либо non-blocking, либо timeout-bounded, поэтому медленный UI
   subscriber не образует очевидного agent mutex cycle
   (`internal/pubsub/broker.go:169-262`).
10. В CLI-provider и обычном Unix shell cancellation есть явные process cleanup
    и backstops. Код документирует deliberate daemon/process-group escape gaps,
    но обычный grandchild-holds-stdio путь закрыт.

## Общая оценка кода

Код движется в правильную сторону: ownership стал явнее, cross-process locking
работает fail-closed, cancellation targets разделены, а диагностика (`sessions
locks/why/watch`, goroutine dumps) стала намного лучше. DB read/write split,
bounded pub/sub, buffered permission responses и process-group cleanup снижают
глобальный head-of-line blocking.

Оставшаяся слабость архитектурная, а не ещё один забытый mutex: существует
несколько независимых реализаций queue/owner/finalizer (`mailbox.submitted`,
`replacement`, `summarizeQueue`, `pending_injects`, detached goroutines,
web-handler tail drains). У них нет единого durable handoff invariant.
Большинство последних регрессий возникло из-за исправления одного exit edge,
когда другой edge сохранял иной контракт.

Центральный инвариант следующего раунда должен быть таким:

> После того как API сообщил, что действие принято, истинно ровно одно:
> (a) за него отвечает живой owner, (b) за него отвечает вновь запущенный
> runner либо (c) существует durable record, который сможет забрать будущий
> worker. Любой выход owner атомарно передаёт или отклоняет всю pending work
> независимо от success, cancellation, error, panic, shutdown или OS-lock
> contention.

## Рекомендуемый порядок исправлений

1. **Закрыть P0-1/P1-1 вместе:** заменить error-path `abandonOwnership` единым
   finalizer, всегда назначающим runner или durable queue owner. Покрыть normal
   turn, preamble, auto-summary, manual summary и OS-lock acquisition errors.
2. **Закрыть P0-2 через durability:** сохранять interrupt/follow-up work до
   подтверждения; использовать claim/lease/ack и повторять lock contention, не
   удаляя source signal заранее.
3. **Сериализовать rerun по ownership** и fail closed, если текущий owner
   фактически не остановился.
4. **Сделать summary config immutable** и использовать cancel-immune bounded DB
   cleanup на её error paths.
5. **Исправить shutdown joins:** учитывать dispatcher goroutines, не закрывать
   DB пока они живы и действительно ограничить внешний shutdown wait.
6. **Связать busy/queued events с mailbox transitions**, затем удалить или
   сузить `QueueMessage` и web-owned summarize-drain contract.
7. После реализации добавить детерминированные тесты каждой проигрывающей
   последовательности, включая отсутствие любого последующего `Run`. Тест,
   доказывающий хранение call для гипотетического будущего owner, недостаточен:
   он должен доказать фактическое выполнение принятого call либо его durable
   claimability.

## Итоговый release gate

Не выпускать этот HEAD как stable. Alpha-релиз допустим только при явном
описании ограничений и сохранении operator recovery. Для stable обязательны
P0-1 и P0-2; P1-1, P1-5 и P1-6 также следует считать blockers, потому что
каждый из них всё ещё даёт тот же наблюдаемый результат, ради которого начат
этот review: сессия выглядит зависшей, её нельзя безопасно продолжить либо её
history изменяется конкурентно.
