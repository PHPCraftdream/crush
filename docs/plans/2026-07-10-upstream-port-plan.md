# План порта апстрим-правок — 2026-07-10

Триаж 237 коммитов `charmbracelet/crush` (v0.72.0 → v0.84.0, merge-base
`61e40556`) по правилам CLAUDE.md. Каждый пункт проверен против реального
кода форка (не по заголовкам коммитов). Правило: cherry-pick по одному,
после каждого — `go build ./...` + тесты затронутых пакетов, отдельный
коммит с `port:`-префиксом и хэшем апстрима.

## Уже есть в форке — НЕ портировать (проверено по коду)

| Upstream | Что | Где в форке |
|---|---|---|
| `1cfa9a15` | пустые ответы саб-агентов | `coordinator.go:2215` — `subAgentOutput` + best-effort cost уже стоят |
| `bb44fb1f` | hooks additionalContext | `hooks/input.go:164,173` — поле уже прокинуто |
| `ffaeec19` | краш при отсутствии default models | `load.go:673-679` — fallback на первую модель уже есть |
| `a08e3329` | UTF-8 в TruncateOutput | форк решил своим `stringext.TruncateAt/TruncateEndAt` |
| `cfdca358` + энричеры ollama/lmstudio/litellm/omlx | discovery-фиксы | `internal/discover/` со `stripV1Suffix` (порт `a1591d78`) |
| `c83418d1`, `d87a632a` | oauth: fabricated lifetime, IsExpired buffer | `oauth/token.go` — формулировки уже совпадают |
| `81170ce5` | reloadMu вместо boolean | свой `reloadMu` в `store.go` |
| `c2be8cbf` | non-interactive env vars в shell | портирован ранее (комментарий в коде) |
| `46c1799a` | fallback title generation | своя multi-model цепочка в `generateTitle` |
| `6292bfa3` | show all providers in models | реализовано в духе форка (atoms) |

## Волна 1 — надёжность агент-харнесса (высокий приоритет)

### 1.1 `21a457d5` — валидация JSON tool-call перед записью в БД
- **Что**: `sanitizeToolInput()` — битый JSON от модели заменяется на `{}`
  + карта `sanitizedToolCalls`, и соответствующий tool result помечается
  ошибкой "arguments were not valid JSON". Без этого битый tool-call
  навсегда заклинивает сессию (перечитывается из БД каждый ход).
- **Форк**: `internal/agent/agent.go` — `OnToolCall` на ~1091, `OnToolResult`
  ниже. Хелпер в конец файла. Проверено: у нас этого нет.
- **Адаптация**: минимальная, наши колбэки структурно совпадают.
- **Тест**: юнит на `sanitizeToolInput` (валидный / битый / пустой вход).

### 1.2 `d3d68045` — guard от session-bricking media на не-vision моделях
- **Что**: в `workaroundProviderMediaLimitations` перед декодированием
  media проверять `largeModel.CatwalkCfg.SupportsImages`; если false —
  текстовый placeholder `[Image/media content not supported by this model]`
  вместо FilePart + синтетического user-сообщения (которое брикает
  text-only модели).
- **Форк**: `internal/agent/agent.go:2617+` — проверено, guard отсутствует,
  тело функции почти идентично апстриму.
- **Тесты**: апстрим добавил +143 строки в `agent_test.go` — портировать
  вместе с фиксом (проверить компилируемость против fantasy v0.25.2).

## Волна 2 — мелкие чистые фиксы

### 2.1 `1535ebb7` — утечка stale ReasoningEffort между провайдерами
- **Форк**: `load.go:766-767` и `810-811` — `if ReasoningEffort != ""`
  без else-ветки. Добавить `else { … = model.DefaultReasoningEffort }`
  в обе. 4 строки.

### 2.2 `f75435a2` — fallback на первый reasoning level
- **Что**: хелпер `effectiveReasoningEffort(model)` — выбранный effort,
  иначе дефолт модели, иначе первый из `ReasoningLevels`; использовать в
  `getProviderOptions` вместо сырого `model.ModelCfg.ReasoningEffort`.
- **Форк**: `coordinator.go:871` (`getProviderOptions`). ВНИМАНИЕ: у форка
  свой CLI-provider effort-механизм (`models_effort.go`) — хелпер применять
  только к API-пути, CLI-провайдеры не трогать.
- **Тесты**: портировать upstream-тесты из `coordinator_test.go` (+31).

### 2.3 `188dea64` — DirTrim ломает не-ASCII имена директорий
- **Форк**: `fsext/fileutil.go:193` — `string(dirs[i][0])` берёт первый
  БАЙТ (кириллица → мусор). Заменить на первый grapheme cluster.
- **Зависимость**: `x/ansi v0.11.7` уже в go.mod — проверить, есть ли в
  этой версии `ansi.FirstGraphemeCluster`; если нет — `rivo/uniseg`
  напрямую (он indirect-зависимость x/ansi) или первую руну через
  `utf8.DecodeRuneInString` (хуже для эмодзи, но приемлемо).
- **Тест**: портировать `fileutil_dirtrim_test.go` (+32, кейсы CJK/эмодзи),
  добавить кириллический кейс.

### 2.4 `78a205cd` — nil guard в copilot initiator transport
- **Форк**: `oauth/copilot/client.go:40` — только `== http.NoBody`, нет
  `== nil` (валидно для GET). 1 строка + портировать их новый
  `client_test.go` (+70).

## Волна 3 — мелкие фичи

### 3.1 `ebd845c0` — session hash в header для cache affinity
- **Что**: `sessionHeaders(sessionID)` → `map[string]string` с хэшем
  сессии; передавать `Headers:` в `AgentStreamCall` (Run + Summarize).
- **Совместимость**: fantasy v0.25.2 УЖЕ имеет `Headers` в
  `AgentStreamCall` (agent.go:264) — проверено. Прямой порт.

### 3.2 `4be77c56` — логировать provider warnings
- **Адаптация**: у апстрима `stepResult.Warnings` (новая fantasy); в
  v0.25.2 вместо этого есть колбэк `OnWarnings func([]CallWarning) error`
  (agent.go:208-209, 283) — регистрировать его в `AgentStreamCall` и
  слогать. Семантика та же.

### 3.3 `c1a48226` — llamacpp-энричер
- **Что**: новый файл в `internal/discover/` + регистрация в `enricher.go`.
  Единственный энричер, которого у нас нет. Прямой порт + тесты.

### 3.4 `363ffec9` — skills discovery от git root (монорепо)
- **Что**: `projectSkillSubdirs` (общий список) + в `ProjectSkillsDir`
  добавлять те же поддиректории от `worktreeRoot(workingDir)`.
- **Форк**: `load.go:629` (вызов) и `load.go:1084+` (функции); наш
  `worktreeRoot` уже существует (и уже обёрнут HideConsoleWindow).
  CLAUDE.md заранее одобряет этот порт.
- **Внимание**: не сломать наш фикс `a88a2578` (исключение per-model
  команд из discovery) — он в другом слое (skills), конфликтов не жду.

## Волна 4 — фича среднего размера

### 4.1 `6242e4f4` — user-level context files
- **Что**: `Options.GlobalContextPaths` (конфиг + schema), выделение
  `loadContextFiles()` в prompt.go, `GlobalContextFiles` в `PromptDat`,
  секция в шаблоне промпта.
- **Форк**: `internal/agent/prompt/prompt.go` наш и разошёлся; наш шаблон
  (`templates/*.tpl`) свой — маппить руками, НЕ брать их шаблон целиком.
- **Риск**: средний (наш prompt — fork-owned). Портировать последним,
  отдельным коммитом, с ручной проверкой итогового системного промпта
  (`crush run --debug` / dump промпта).

## Волна 5 — EVAL (решение по каждому отдельно, после Волн 1-4)

Исследование (task #34) проведено read-only на реальном коде форка (плюс
временный repro-тест для 5.1, удалён после проверки, не закоммичен).

| # | Upstream | Вопрос | Вердикт |
|---|---|---|---|
| 5.1 | `d4dc84e9` | дедлок на старте при невалидном model ID | **УЖЕ ПОКРЫТО ИНАЧЕ.** Repro: временный `crush.json` с `models.large`/`models.small` → несуществующий model ID у существующего провайдера (openai), запуск реального `config.Load(...)` с таймаутом 8с — вернулся `err=nil` за ~1.5с, фолбэк на дефолтную модель, зависания нет. Причина: апстримовский баг — реэнтерабельный `writeMu.Lock()` (persist=true внутри уже залоченного Load). У форка `configureSelectedModels` в `reloadFromDiskLocked` (`store.go:815`) вызывается с `persist=false`, а в `autoReload` (`store.go:850`) используется `reloadMu.TryLock()` (не блокирующий) — самозахват мьютекса структурно невозможен. Портировать нечего. |
| 5.2 | `90197756`+`3cd88ac8`+`23157b6a` | централизация 401-retry | **УЖЕ ПОКРЫТО.** `internal/agent/coordinator.go` содержит `runWithUnauthorizedRetry` (строка ~2100) с точно такой же сигнатурой/семантикой, применённый во всех трёх точках — `Run` (~622), `Summarize` (~2027), `runSubAgent` (~2229) — включая условие «notify только если после retry всё ещё 401». Это было портировано ранее в коммите `a1591d78` ("selectively implement upstream fixes"). Дифф апстрима 1:1 совпадает с текущим кодом форка. |
| 5.3 | `de679203` | single-flight refresh токенов между процессами (305 строк) | **PORT (частично, опционально).** Форковский `RefreshOAuthToken` (`store.go:359`) уже покрывает "пере-проверка диска перед/после exchange" (adoptable disk token), что закрывает большинство практических гонок. Но у форка НЕТ (1) in-process `singleflight.Group` — конкурентные горутины в одном процессе всё ещё могут запустить параллельные exchange до первой записи на диск, и (2) кросс-процессного advisory-лока с deadline 45с на время самого HTTP-обмена — окно гонки между "прочитали диск" и "выполнили exchange" не закрыто никаким локом, только эвристикой sequential-read. План (если решим портировать): добавить `singleflight.Group` поле в `ConfigStore` + обернуть текущий `RefreshOAuthToken` в `s.refreshSF.Do(key, ...)` — это дешёвая часть (~10 строк), не трогая остальную дисковую логику. Кросс-процессный `lock.File` — большая часть, апстримовская реализация зависит от их `internal/lock` пакета (в SKIP-списке форка как "их lock-пакет"), нужна отдельная оценка есть ли у форка эквивалент (`sessions_*.go` lock-file механизм) для повторного использования. Не блокер — только у Hyper (ротирующиеся refresh-токены) есть реальный риск; текущая дисковая эвристика форка снижает, но не устраняет гонку. |
| 5.4 | `d3af321b` | изоляция child-процессов от сессии | **УЖЕ ПОКРЫТО.** `internal/shell/exec_unix.go` содержит `isolateProcess` (`Setsid: true`) и `processGroupExecHandler` с negative-PID kill для всей группы процессов — семантически идентично апстриму (с форковским комментарием почему: нет TUI/TTY, но нужна изоляция от SIGINT/SIGTERM и полная очистка поддерева при отмене). `internal/shell/exec_windows.go` имеет аналог с `HideWindow: true` (форк-специфичная адаптация под Windows/no-console). `dispatch.go:200` тоже вызывает `isolateProcess(cmd)` для shebang-скриптов, как и апстрим. Ничего портировать не нужно. |
| 5.5 | `67f50014` | bounded glob walk в fallback | **PORT (реальный гэп, не srочно).** У форка есть rg fast-path с лимитом (`internal/agent/tools/glob.go`), НО fallback-ветка (`internal/fsext/fileutil.go:98` `globWithDoubleStar`, вызывается когда rg недоступен/упал) использует `fastwalk.Walk` с `Follow: true` (следует симлинкам — риск бесконечного цикла или выхода за пределы проекта) и БЕЗ префикс-скоупинга по литеральной части паттерна (сканирует весь `searchPath` целиком, фильтруя постфактум) и без per-call `context.WithTimeout`. Есть частичная защита — `limit*2` ранний `filepath.SkipAll`, так что по количеству совпадений не unbounded, но по объёму сканируемых файлов/памяти при большом дереве (например `$HOME`) — да, уязвим. План: добавить `context.WithTimeout` вокруг вызова `globFiles`/`GlobGitignoreAware`, `Follow: false` в fastwalk.Config (или явный флаг), и опционально префикс-скоупинг аналогично апстримовскому `filepathext.SplitGlobPrefix`. Не критично для большинства сценариев форка (агенты работают в git-репах с `.gitignore`), но стоит сделать отдельным небольшим коммитом. |
| 5.6 | `a06fd034` | schema reflection для provider options | **УЖЕ ПОКРЫТО.** `internal/cmd/schema.go` содержит `setProviderTypeEnum` и `internal/discover/enricher.go` содержит `RegisteredProviderTypes()` — код 1:1 совпадает с апстримовским диффом (включая doc-комментарии, с добавлением litellm/lmstudio в форковский комментарий). Портировано ранее. |
| 5.7 | `a9e3a57f` | shell persistence при умершей сессии | **SKIP, замаппить нельзя без реструктуризации.** Апстримовский фикс живёт в `internal/backend/agent.go` и `internal/workspace/app_workspace.go` — оба пакета **отсутствуют у форка** (`ls internal/` не показывает ни `backend`, ни `workspace`; форк использует `internal/server` + `internal/app` для этого слоя). У форка также нет `internal/shell/persist_message.go` или аналогичного централизованного `PersistOutput` хелпера — паттерн "shell.PersistFunc с FK-error-based skip" в форке просто не существует как отдельная абстракция. Если форк вообще персистит bang-mode shell-команды в БД по SessionID из другого процесса (нужна отдельная проверка — не входит в объём этой задачи), логика легла бы в `internal/server/` или `internal/agent/`, но это отдельная feature-задача, не 1:1 порт. SKIP как есть; при реальном баг-репорте — переоценить. |
| 5.8 | провайдер-фиксы: `d341d84b`, `3ac7f1c4`, `b7f4ad6c`, `f6b8592a`, bedrock-eu (`179a6f35`,`57ae2b44`,`36f0976c`), `9886d223`, `d0dc9fc9` | используются ли провайдеры в форке | Построчно: **`b7f4ad6c` (qwen3.7-max)** — УЖЕ ПОРТИРОВАН, `coordinator.go:137-141` содержит `opencodeMessagesModels` с явным комментарием `// Ported from upstream b7f4ad6c (#3040)`. **`f6b8592a` (alibaba /messages)** — УЖЕ ПОРТИРОВАН, `coordinator.go:973,1106` содержат `case string(catwalk.InferenceProviderAlibabaSingapore)` с идентичной логикой thinking/extra_body. **`d341d84b` (alibaba deepseek thinking traces)** — НЕ портирован: апстримовский дифф добавляет ветку для `catwalk.InferenceProviderAlibabaUS`, которого у форка нет вообще (только `AlibabaSingapore`); зависит от цепочки `4882cbc0`→`73777d5a`→`ebf6e826` (alibaba-us провайдер, вероятно требует бампа catwalk) — explicit-решение с бампом версии, не делать самостоятельно. **`3ac7f1c4` (baseten)** и **`9886d223` (fireworks)** — провайдер вообще не используется в форке (`grep -rli "baseten\|fireworks" internal/` — ноль хитов в non-test коде, `catwalk.InferenceProviderFireworks` отсутствует в coordinator.go) → SKIP, портировать при реальной надобности. **bedrock-eu кластер (`179a6f35` и соседи)** и **`d0dc9fc9` (gpt-5.6)** — оба являются чистыми `go.mod`/`go.sum` бампами fantasy/catwalk без изменений в crush-коде → SKIP, требуют явного запроса на бамп версии (запрещено самостоятельно по CLAUDE.md/global-инструкции). |
| 5.9 | `a07d304f` | hyper credits refresh, фикс целиком в `internal/ui/model/ui.go` (TUI) | **SKIP подтверждён.** `internal/ui/` у форка существует, но это ДРУГОЙ пакет — только OS-нативные desktop-уведомления (`internal/ui/notification/`: `icon_darwin.go`, `native.go`, `notification.go`, значки). Файла `internal/ui/model/ui.go` или папки `internal/ui/model/` в форке нет (`ls internal/ui/` показывает только `notification`), `internal/tui/` тоже отсутствует. Апстримовский фикс адресован Bubble Tea TUI, которого у форка нет — SKIP корректен. |

## SKIP (без изменений против триажа)

TUI целиком (bangmode ~20, scrollbar/scrolling, dialogs, pills, clipboard,
рендеринг), multi-client server (~30: sockets, data-dir lock, prompt
lifecycle, HTTP 202), CLA (~20), auto-update, версии-теги, deps-бампы,
`7ce09361` (их lock-пакет), notifications (свой notify), herdr,
release-инфра.

## Порядок работы

1. Волна 1 → Волна 2 → Волна 3 → Волна 4, по одному коммиту на пункт,
   формат сообщения: `port(<area>): <suть> [upstream <hash>]`.
2. После каждого пункта: `go build ./...` + `go test` затронутых пакетов.
3. Волна 5 — после лендинга 1-4: каждый EVAL закрывается вердиктом
   (PORT-adapted / уже покрыто / SKIP c причиной) в этом же файле.
4. В конце: полный тест-прогон + pre-push hook, обновить CHANGELOG.fork.md
   (раздел Chronological commit log) одним сводным блоком.
