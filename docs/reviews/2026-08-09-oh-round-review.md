# Whole-round review by `@oh` (Opus) — release-concurrency round #328–#336

Дата: 2026-08-09

Проверенный диапазон: `d5bbfeae..7d8072e1` (весь раунд как единый совокупный
diff, а не по одной задаче).

Автор: агент `@oh` (Opus, effort=high), запущенный оркестратором как финальный
шаг `/babygoal`.

Режим: **динамический** — агент самостоятельно прогнал build / vet / lint /
полный `go test` / `-race` и написал два одноразовых probe-теста, чтобы
воспроизвести две находки (probe-файлы удалены после проверки, в дерево не
попали).

> **Провенанс.** Этот отчёт `@oh` вернул текстом в финальном сообщении и файлов
> не создавал — он явно об этом сообщил. Отдельный отчёт
> `2026-08-09-release-concurrency-followup-review.md` написан **другим** агентом
> (статическое ревью, без прогона тестов) и содержит другой набор находок.
> Оба отчёта дополняют друг друга; расхождения между ними отмечены в конце.

## (a) Вердикт

**NO-GO** для релиза этого раунда как есть. Две из девяти правок вносят *новые*
дефекты, которые хуже закрытых ими пробелов, и обе — в P0-пути, ради которого
раунд и затевался. Всё при этом собирается и проходит build/vet/lint/test,
включая `-race`, — тулчейн это не ловит.

Остальная часть раунда направлена верно: P1-4, P1-6, P2-1 (execution), P2-3
действительно закрыты невырожденными тестами.

## (b) Находки

### BLOCKER-1 — `restartOrphanedWithRetry` повторно ставит в очередь уже выполненный call → неограниченное дублирование выполнения

`internal/agent/agent.go:1088-1131` (коммит `37f1820d`)

Retry-цикл делает `break` на **любой** ошибке, не являющейся
`SessionLockBusyError`, и затем **проваливается** в
`a.getMailbox(call.SessionID).queue(call)`. Комментарий на :1119-1121
утверждает, что «The call was never durably queued … because tryReserveSession
made it the immediate owner» — это верно **только** для выхода по
lock-contention. При обычной ошибке turn'а (5xx провайдера, сбой записи в БД,
ошибка summary — то есть ровно тот класс ошибок, ради которого существует P0-1)
`a.Run` возвращает ошибку **после** того, как создал user-сообщение и отстримил
полный turn, и уже выполненный call кладётся обратно в `mb.submitted`.

Воспроизведено временным probe (удалён):

```
provider calls=11, queued in mb.submitted=1, persisted 'dup-probe' user rows=2
```

Последовательность: turn A владеет сессией, сообщение B в очереди → A падает с
5xx провайдера → `abandonOwnershipWithHandoff` → `restartOrphanedWithRetry([B])`
→ detached `Run(B)` полностью выполняет B, затем возвращает ошибку → `queue(B)`
→ следующий несвязанный `Run` для этой сессии дренит B и выполняет его
**повторно** (вторая дублирующая user-строка), затем падает и ставит его в
очередь **снова**. Каждый последующий turn в этой сессии переигрывает устаревший
prompt: дублирующиеся строки транскрипта, удвоенный расход токенов, сходимости
нет. Это прямое нарушение центрального инварианта самого ревью («ровно одно из
(a)/(b)/(c)»).

**Направление фикса:** проваливаться в `queue(call)` только тогда, когда цикл
исчерпал retry именно по `SessionLockBusyError`; при non-retryable ошибке —
залогировать и отбросить (call уже выполнился).

### BLOCKER-2 — та же форма в `startDetachedRun`: pending-inject row пересоздаётся после того, как сообщение уже выполнилось

`internal/agent/coordinator.go:2263-2287` (коммит `37f1820d`)

Идентичный `break`-then-fall-through: после non-retryable ошибки код
пересоздаёт строку `pending_injects` со свежим UUID. Если detached run выполнил
interrupt-сообщение и *затем* упал, следующий interrupt-tick заново
консьюмит пересозданную строку, **отменяет тот turn, который в этот момент
выполняется**, и повторно прогоняет то же сообщение. Тот же класс, что
BLOCKER-1, плюс отмена постороннего живого turn'а.

Отдельно: новый doc-комментарий `internal/session/session.go:221-226` («The row
is recreated in `startDetachedRun` if the detached run fails») верен только для
*idle*-ветки `requeueInterruptMessage`; в owned-ветке `ConsumeInterruptInject`
по-прежнему удаляет строку до выполнения, и `InjectID` до `startDetachedRun`
не доходит. Дыра durability в P0-2 сужена, но не закрыта.

### MAJOR-1 — реордер release в P1-2 позволяет уходящему holder'у затереть метаданные *живого* нового holder'а

`internal/session/lock.go:289-311` и `internal/session/lock.go:365-380`
(коммит `36c9a3b9`)

`clearHolderMetadata` теперь выполняется **после** `unlockFile`+`Close`,
переоткрывает путь `O_RDWR`, делает `Truncate(0)` и `os.Remove` sidecar'а.
Исходный код делал это намеренно *под удерживаемым OS-локом* — именно чтобы
не гоняться с новым holder'ом. Воспроизведено детерминированно на Windows через
новый seam `clearHolderMetadataFn` (probe удалён):

```
new holder PID before=34260 after=0 ; sidecar after err=...cannot find the file specified
```

Следствие: `SessionLockBusyError.HolderPID` читается как 0,
`session.ReadLockPID` — как 0, поэтому
`internal/cmd/sessions_kill.go:101-102,235-238` получает `livePID=0` и от probe,
и от fallback, а `forceKillHolder` превращается в no-op — `crush sessions kill
<id>` больше не может убить живого holder'а. `sessions why`/`locks`/`watch`
теряют PID и строку ELAPSED/BUDGET. Окно обычно микросекундное, но расширяется
ровно в сценарии зависшей FS / AV / SMB, ради которого P1-2 и писался — то есть
аварийный рычаг ломается именно тогда, когда он оператору нужен. Ни один тест
раунда не покрывает направление затирания (`TestP1_2_ReleaseClearsMetadata`
проверяет только сценарий без contention).

**Направление фикса:** реордер сохранить (unlock первым — правильно), но
пост-unlock очистку сделать условной: неблокирующе перезахватить OS-лок перед
truncate и пропустить очистку, если лок занят.

### MAJOR-2 — P1-5 это bounded *wait*, а не join диспетчеров; закрытие БД теперь может пережить `Shutdown`

`internal/app/app.go:174-207`, `internal/app/app.go:1810-1823`,
`internal/agent/agent.go:4292-4304` (коммит `334a9dc4`)

P1-5 из ревью требовал (i) настоящий join-примитив на живых диспетчерах и
(ii) «не начинать закрытие БД, пока writer'ы ещё могут работать». Не сделано ни
то, ни другое. `CancelAll` по-прежнему просто поллит `IsBusy()` 5 секунд;
`stillBusy` уходит только в `slog.Warn`. А новая обёртка DB-cleanup делает при
таймауте строго *хуже*: `db.Release`/`sql.DB.Close` теперь выполняются в
отсоединённой горутине, поэтому `Shutdown()` возвращается (и процесс выходит),
пока закрытие SQL-пула ещё гоняется с той агентской работой, что пережила
grace-период. Заголовок коммита «shutdown joins live dispatchers» не
соответствует тому, что делает код.

### MAJOR-3 — удаление web-дрейна summarize осиротило событие `summarize_queued=false`

`internal/server/handlers.go:303-312` (коммит `26b18e55`)

Удалённый хвост был единственным производителем
`EventSummarizeQueued{Queued:false}` на success-пути (второй —
`handleCancelQueuedSummarize`). `abandonOwnershipWithHandoff` запускает
summarize detached и не эмитит ничего. `handleListSessions` ресинхронизирует
`agent_busy` по сессиям, но **не** `summarize_queued`, а у
`Coordinator.SummarizeQueued()` нет ни одного вызова со стороны сервера — так
что `web/src/store.ts:$summarizeQueued` / `ChatToolbar.tsx:151` навсегда
залипают в «compact queued» после авто-дренированной компакции, без
самовосстановления и без пути через reload.

(Родственный пробел по `agent_busy` — detached run захватывает mailbox раньше,
чем handler владельца проверит `IsSessionBusy`, поэтому `Busy:false` не
рассылается никогда — самовосстанавливается 5-секундным поллом `list_sessions`,
поэтому это только **minor**.)

### MAJOR-4 — в P1-3 всё ещё есть окно рассогласования model/provider-options, просто более узкое

`internal/agent/coordinator.go:2502-2519` + `internal/agent/agent.go:3163`
(коммит `d1c9ab45`)

`coordinator.Summarize` снимает snapshot `agentModel` и вычисляет из него
`opts`; `runSummarize` затем делает **второе, независимое** чтение через
`a.resolveTurnConfig(SessionAgentCall{})`, чтобы получить `cfg.largeModel`.
`SetModels`, попавший между этими двумя чтениями, даёт ровно тот дефект, который
описан в P1-3: стриминг моделью X под provider-опциями, вычисленными для модели
Y. Snapshot нужно протаскивать, а не переразрешать. Собственный комментарий кода
на :3157-3162 признаёт, что это «минимальный фикс».

### MINOR-1 — заявленный инвариант безопасности `popAllSubmitted` неверен

`internal/agent/mailbox.go:1134-1155`. Документация говорит, что метод безопасен
без проверки epoch, «потому что новый submit не может попасть на `mbIdle`».
`mailbox.queue()` (используемый `QueueMessage` и самим fallback'ом из
BLOCKER-1) добавляет в очередь независимо от состояния, а между unlock'ом
`abandonOwnership` и захватом лока в `popAllSubmitted` может появиться новый
владелец и получить queued-работу. Сегодняшнее следствие — переупорядочивание, а
не потеря (украденные call'ы ре-сабмитятся за новым владельцем), но инвариант в
том виде, как он записан, не держится, и `popFirstSubmitted`/`popAllSubmitted`
остаются единственными мутаторами mailbox без epoch.

### MINOR-2 — несинхронизированный test seam в production-коде

`internal/agent/agent.go:94` — package-level `func`-переменная
`testStreamWatchdogTick` пишется тестом
`TestP2_3_ManualCompactionWatchdogCatchesIdleStall` (который `t.Parallel()`),
пока `runTurn`/`runSummarize` читают её из горутин других параллельных тестов.
Латентный `-race` flake; ни в одном из двух race-прогонов не выстрелил.

### Вырожденные / неверно маркированные тесты (найдены, **не** исправлены)

- `internal/app/p1_5_shutdown_test.go:341` `TestP1_5_DBCleanupRespectsContext` —
  **mirror-тест**: он переизобретает обёртку goroutine+select из app.go *внутри
  собственной записи `cleanupFuncs`* и никогда не трогает настоящую closure
  DB-cleanup'а. Проходит идентично при полностью откаченной production-правке.
  Это ровно тот анти-паттерн, который запрещают doc-комментарии самого репозитория
  (задача #243).
- `internal/app/p1_5_shutdown_test.go:149` `..._CleanupWaitsForCancelAll` —
  проверяет порядок, который был верен и до раунда; ни для чего изменённого это
  не регрессионный тест.
- `internal/app/p1_5_shutdown_test.go:291` `..._LogsWarningWhenStillBusy` —
  никогда не проверяет само предупреждение; единственная проверка — «вернулось
  за <12s», что было верно и до фикса.
- `internal/agent/p1_3_p1_4_regression_test.go:40`
  `TestP1_3_SummaryUsesImmutableSnapshot` — вызывает
  `runSummarizeSilent(..., snapshotModel, snapshotPrefix)` напрямую, то есть
  доказывает лишь то, что функция использует собственные параметры. Он не
  задействует ни одной production call-site, разрешающей snapshot, а «реверт», от
  которого он якобы защищает, — это изменение сигнатуры (ошибка компиляции, а не
  падающая проверка).
- `internal/agent/p0_2_cross_process_test.go:182-240` (добавлено `7d8072e1`) —
  фаза 2 **сама создаёт** строку `injectPhase2` и вызывает `startDetachedRun`
  напрямую; она не задействует цепочку recreate→tick→execute, заявленную в
  сообщении коммита, и прошла бы и с закомментированным блоком recreate. Её
  проверка `foundOriginalMessage` тавтологична (тест сам создал это сообщение).
  Сигнал несёт только `providerCalls >= 1`. То есть заявление самого
  P0-audit-раунда про «persistence is not execution» для последовательности 4
  выполнено наполовину.
- `internal/agent/p0_2_regression_test.go:43` (вторая цель `7d8072e1`) —
  усилен по-настоящему; проверка истории сообщений реальна. Хорошо.
- `internal/agent/p2_1_regression_test.go:33` и
  `internal/agent/p2_3_regression_test.go:189` — по-настоящему невырожденные
  (реальные счётчики провайдера, реальный bounded-возврат). Хорошо.

### Комбинированный losing-сценарий, не покрытый нигде в раунде

Ни один тест не гоняет **две** из девяти последовательностей ревью конкурентно.
Важнейшая — композит из BLOCKER-1: *non-cancel ошибка владельца (P0-1) → handoff
detached run (P0-2) → этот detached run тоже падает → requeue*. Все существующие
тесты гоняют handoff с **успешным** detached run — именно поэтому девять раундов
узкой верификации это пропустили. Регрессионный тест должен проверять
`len(mb.submitted) == 0` и ровно одну персистентную user-строку после detached
run, который выполнился и затем упал.

## (c) Собственные прогоны `@oh`

| Проверка | Область | Результат |
|---|---|---|
| `go build ./...` | всё | **PASS** |
| `go vet` | `agent/… session/… app/… server/… db/…` | **PASS** |
| `golangci-lint@v2.10 run` | те же 5 пакетов | **PASS — 0 issues** |
| `go test -count=1` (не `-short`) | те же 5 пакетов | **PASS** (все `ok`) |
| `go test -race -count=1 -run "P0_\|P1_\|P2_\|Mailbox\|Ownership\|Handoff\|Shutdown\|Compact\|Summarize"` | agent, app, session | **PASS**, race-репортов нет |
| `go test -race -count=1` (полный, без кеша) | agent/…, app, session, server | **PASS**, race-репортов нет — agent 185s, session 52s, app 37s, server 21s |

Систематический `-race` пробел, не закрытый за весь раунд, теперь закрыт:
ничего в раунде детектор не триггерит. Оба блокера — логические дефекты, которые
детектор и не нашёл бы.

## (d) Исправления тестов, внесённые `@oh`

**Никаких.** Написаны два одноразовых probe
(`internal/agent/zz_probe_double_exec_test.go`,
`internal/session/zz_probe_lock_clobber_test.go`), оба подтвердили падение на
текущем `HEAD`, оба удалены. Production-код не тронут. Перечисленные вырожденные
тесты нетривиальны для починки (P1-5 нужно переписать против настоящей cleanup-
closure из `App.New`, P1-3 — против реального сценария с конкурентным
`SetModels` во время turn'а), поэтому оставлены на отдельную задачу.

## Соотношение с параллельным статическим ревью

`2026-08-09-release-concurrency-followup-review.md` (другой агент, только
статический анализ) сходится с этим отчётом по пяти пунктам: затирание
метаданных лока (там P2-1), shutdown не является join'ом (P1-3), сломанные
web-события summarize (P1-2), рассинхрон snapshot'а manual summary (P1-1),
глобальные test seam'ы под `t.Parallel` (P2-2).

Существенно расходятся они в характере дефекта P0-2:

- **этот отчёт** описывает **дублирующее выполнение** (call уже выполнился и
  переигрывается);
- **статический отчёт** описывает **потерю работы** (call не получает runner'а
  вообще).

Противоречия нет: это разные ветки одного и того же хвоста
`restartOrphanedWithRetry`/`startDetachedRun`. Наивная починка одной ветки
(«просто отбрасывать call») усугубит другую. Обе должны разводиться явно.

Уникально для статического отчёта и здесь не отмечено: незакрытость
`SessionLock.Release()` как источника вечного `mbOwned`/`mbReleasing`,
одно-попыточный `restartOrphaned` в `drainOrReleaseMerged`, отсутствие pump'а у
восстановленной `pending_injects`, недоказанность hard-abort для провайдеров
кроме `openaicompat`.
