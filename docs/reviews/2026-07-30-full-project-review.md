# Полное ревью: доработки #127–#151 + широкое ревью проекта

**Дата:** 2026-07-30
**База:** `main` @ `054d23d6` (8 коммитов последних двух раундов поверх исходного батча из 11 фиксов)
**Режим:** zero-trust — перечитаны реальные диффы (`git show`) и текущее состояние файлов, а не commit-месседжи
**Изменений в код не вносилось.** Это чисто ревью-документ.

---

## 0. Санити-прогон

| Проверка | Результат |
|---|---|
| `go build ./...` | ✅ exit 0 |
| `go vet ./...` | ✅ exit 0 |
| `golangci-lint run` (v2.10, как в `.githooks/pre-push`) | ✅ **0 issues** |
| `go test -race -count=1` по `internal/session`, `internal/config`, `internal/server`, `internal/shell`, `internal/agent/agentguard`, `internal/db`, `internal/log` | ✅ все `ok`, ни одной гонки |

Полный `go test ./...` намеренно не запускался (правило CLAUDE.md для суб-агентов —
оркестратор гоняет полный набор сам).

---

## 1. Проверка недавних доработок (по пунктам задания)

### 1.1 Утечка реального глобального конфига в тесты — закрыта **не до конца**

Что сделано в `054d23d6` — корректно и по делу: `models_use_test.go`,
`providers_test.go`, `mcp_test.go`, `projects_test.go` теперь изолируют **обе**
резолюции (`CRUSH_GLOBAL_DATA`/`XDG_DATA_HOME` **и**
`CRUSH_GLOBAL_CONFIG`/`XDG_CONFIG_HOME`), причём в **разные** подкаталоги —
это правильно, потому что `lookupConfigs` (`internal/config/load.go:907-913`)
кладёт в список путей и `GlobalConfig()`, и `GlobalConfigData()`, а `go-jsons`
мержит массивы, а не заменяет их.

**Но та же дыра осталась ещё в двух местах** — см. находки **H-1** и **M-6** ниже.

### 1.2 Транзакционность session fork — обе ветки атомарны, но **разъехались по смыслу**

Проверено построчно:

* `internal/session/session.go:318-431` (`ForkSession`, серверный путь) — один
  `BeginTx`, `defer tx.Rollback()`, все `UPDATE`/`CreateMessage` через `qtx`,
  `Commit` в конце, перечитывание после коммита, `Publish` **после** коммита.
  Атомарность корректна.
* `internal/cmd/sessions_fork.go:106-207` (`forkSessionCLI`) — та же форма,
  тоже корректна. `resolveSessionID` вынесен наружу транзакции намеренно
  (hash-prefix lookup), и это ок: источник перечитывается внутри tx.

Откат действительно атомарен во всех кейсах, включая падение на N-м сообщении.
**Проблема не в транзакционности, а в дрейфе набора копируемых полей** — см. **M-1**.

### 1.3 HTTP streaming логгер (`internal/log/http.go`) — корректен

`teeBody` (строки 208-255): `Read` копирует префикс под `mu`, `Close`
идемпотентен (`closed` + `sync.Once`), лог эмитится один раз на `Close`,
живой стрим не материализуется. `debugEnabled` проверяется один раз — на
не-debug уровне тело вообще не трогается. `RetryTransport` буферизует тело
только для реально ретраибельных запросов. Возражений нет; одно мелкое
замечание — **L-4**.

### 1.4 WebSocket ring buffer (`internal/server/hub.go`) — инварианты держатся

`replayBuffer` (44-111): backing-слайс аллоцируется один раз, `push`/`evictHead`
— чистая индексная арифметика, `evictHead` обнуляет ссылку (GC), байтовый
бюджет пересчитывается симметрично при вставке/эвикции, `count > 1` не даёт
выкинуть только что вставленное событие. `replayMaxEventSize (1 MiB) <
replayByteBudget (16 MiB)` — инвариант, на котором держится терминирование
цикла эвикции, выполняется. Всё корректно. Мелочи — **N-1**, **N-2**.

### 1.5 Deploy atomic rename — остались два мелких края

`internal/deploy/deploy.go:72-87` (`SwapRenameAside`) — логика отката верная,
все три исхода различимы и текст ошибки катастрофического случая называет оба
пути. `SweepRenameAsideLeftovers` корректно не трогает свежие `.new-*`.
Оставшиеся края — **L-5** и **N-6**.

### 1.6 Cross-process lock конфига — защищает, deadlock'а нет, **но есть новый stall-риск**

Проверено:
* Порядок блокировок консистентен: `publishMu → diskWriteMu → file lock`,
  нигде не встречается обратный.
* Само-deadlock по `publishMu` невозможен: `autoReload` (`store.go:1424`)
  использует `TryLock`, а не `Lock` — реэнтрантный вызов
  `Load → configureProviders → RemoveConfigField → autoReload` просто
  пропускает reload. Это правильно и явно задокументировано.
* Файловый лок действительно межпроцессный (flock / LockFileEx по отдельным
  file description'ам конфликтуют даже внутри одного процесса) — тест
  `store_crossprocess_test.go` моделирует это адекватно.
* Решение не удалять `.lock`-сайдкар обосновано корректно (Windows без
  `FILE_SHARE_DELETE`).

**Новый риск, который фикс внёс:** ожидание межпроцессного лока до 30 с
происходит, когда вызывающая горутина уже держит `publishMu`. См. **H-2**.

---

## 2. Находки

Приоритеты: **Critical** (потеря данных/крэш в проде, чинить немедленно) →
**High** → **Medium** → **Low** → **Nit**.

Critical не найдено.

---

### High

#### H-1. Та же утечка хост-конфига осталась в `internal/agent/coordinator_test.go` (4 места)

**Файлы:** `internal/agent/coordinator_test.go:1651`, `:1841`, `:2072`, `:2223`

Все четыре хелпера делают только:

```go
t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())
cfg, err := config.Init(env.workingDir, "", false)
```

`config.Init → Load → lookupConfigs` (`internal/config/load.go:907-913`) тянет
**и** `GlobalConfig()`, который управляется `CRUSH_GLOBAL_CONFIG`/
`XDG_CONFIG_HOME` — а они здесь не изолированы. То есть реальный
`~/.config/crush/crush.json` оператора (по CLAUDE.md — именно там живут
боевой API-ключ и определения MCP-серверов) читается и мержится в конфиг теста.

Комментарий в самом тесте («Isolate from the host machine's real global config
… without this, `config.Init` falls back to `GlobalConfigData()`») описывает
ровно ту половину проблемы, которую `054d23d6` уже закрыл в `internal/cmd` —
вторая половина здесь не закрыта.

**Последствия:** тесты вроде «worker НЕ сконфигурирован» (`:1841` явно делает
`delete(coord.cfg.Config().Models, SelectedModelTypeWorker)` именно потому, что
хост протекает) зависят от содержимого машины разработчика; на CI зелено, у
оператора — может быть красно или наоборот ложно-зелено. Сетевого риска здесь
нет (`config.Init` не поднимает `mcp.Initialize`, в отличие от
`app.New()`), поэтому это High, а не Critical.

**Фикс:** вынести общий хелпер (например `isolateAllGlobalConfigPaths(t)`),
ставящий `CRUSH_GLOBAL_DATA`+`XDG_DATA_HOME` в один tmp-подкаталог и
`CRUSH_GLOBAL_CONFIG`+`XDG_CONFIG_HOME` — в **другой**, и вызвать его во всех
четырёх местах. Лучше — один экспортируемый тест-хелпер на репозиторий, чтобы
следующий такой тест не пришлось искать грепом.

---

#### H-2. `withConfigWriteLock` может держать `publishMu` до 30 секунд

**Файл:** `internal/config/store.go:272-298`, путь входа —
`internal/config/load.go:141-142` + `:317`

```go
store.publishMu.Lock()          // load.go:142, держится весь Load
defer store.publishMu.Unlock()
...
cfg.configureProviders(...)     // load.go:155
    → store.RemoveConfigField(ScopeGlobal, "providers.anthropic")  // load.go:317
        → s.withConfigWriteLock(path, ...)                          // store.go:272
            → session.AcquireFileLockContext(ctx /* 30s */, path+".lock")
```

До `b53cbf3d` эта критическая секция была локальным дисковым I/O (миллисекунды).
Теперь она может блокироваться до `configWriteLockTimeout = 30s`, если
соседний `crush`-процесс держит `crush.json.lock` (или завис с ним —
отладчик, приостановленный shell, замёрзший сетевой диск). Всё это время
`publishMu` удерживается, а значит в этом процессе встают: `ReloadFromDisk`,
`updateConfig`, `SetSkipPermissionRequests`, `CaptureStalenessSnapshot` — то
есть весь конфиг-субсистема, включая старт приложения.

Дедлока нет (цикла ожидания не образуется), но это отказ доступности,
которого до фикса не существовало. Для форка, чей сценарий — N параллельных
`crush run` на одной машине, вероятность встретить контенцию на глобальном
`crush.json.lock` не нулевая.

**Фикс (варианты, по возрастанию инвазивности):**
1. Отдельный, гораздо более короткий таймаут для пути, вызываемого изнутри
   `Load`/`reloadFromDiskLocked` (например 2 с), с деградацией «не смогли
   удалить ключ на диске — пропускаем, in-memory уже почищен через
   `Providers.Del`». Семантика этого конкретного вызова (удаление устаревшего
   `providers.anthropic`) это допускает — он и сейчас игнорирует ошибку.
2. Вынести конкретно этот `RemoveConfigField` из-под `publishMu` (отложить в
   пост-`Load` шаг).
3. Логировать факт ожидания дольше N мс, чтобы такой stall был диагностируемым,
   а не «crush просто не стартует».

---

#### H-3. Панику в любом WS-хендлере не ловит никто — падает весь процесс сервера

**Файлы:** `internal/server/server.go:140-154` (комментарий прямо признаёт
пробел), `internal/server/handlers.go:40-108`

Каждое входящее WS-сообщение уходит в `go handleX(ctx, a, c, msg)` — около
40 точек. Ни в `readPump`, ни в `handleIncoming`, ни в самих хендлерах нет
`recover()`. Любая паника (nil-deref на неожиданном payload, индекс за
границей, паника в нижележащем слое) убивает **весь процесс `crush web`**,
вместе с работающими агентскими сессиями.

В коде есть fork-merge-note, который прямо это фиксирует как известное место
(«The equivalent in our architecture would be a `recover()` inside
`handleIncoming` or this for-loop»), но фикса нет.

Дополнительно: горутины на сообщение ничем не ограничены — клиент (или баг в
UI) может выстрелить тысячами сообщений и получить тысячи одновременных
горутин, каждая из которых лезет в SQLite, у которого `SetMaxOpenConns(1)`
(см. **M-5**).

**Фикс:** обёртка-диспетчер вида

```go
func safeGo(name string, fn func()) {
    go func() {
        defer func() {
            if r := recover(); r != nil {
                slog.Error("ws: handler panic", "handler", name, "panic", r,
                    "stack", string(debug.Stack()))
            }
        }()
        fn()
    }()
}
```

плюс (отдельно) семафор на N одновременных хендлеров на соединение.

---

#### H-4. `permissionService.Request` держит `requestMu` всё время ожидания ответа

**Файл:** `internal/permission/permission.go:244-350`

```go
s.requestMu.Lock()
defer s.requestMu.Unlock()   // держится до самого конца, включая:
...
fileInfo, err := os.Stat(opts.Path)              // I/O под локом
...
s.q.MatchSessionPermission(ctx, ...)             // запрос в SQLite под локом
...
select {                                         // БЛОКИРУЮЩЕЕ ожидание
case <-ctx.Done():
case granted := <-respCh:                        // ответа человека — под локом
}
```

Комментарий у поля (`// used to make sure we only process one request at a
time`) это описывает как намерение, и для одномодального TUI это было
осмысленно. Но:

* Форк позиционируется как N параллельных сессий, а `permissionService` —
  **один на процесс**. Один неотвеченный запрос разрешения в сессии A
  подвешивает запросы разрешения **всех** остальных сессий, пока не
  сработает их собственный ctx.
* Веб-UI не имеет ограничения «одна модалка», так что архитектурная причина
  сериализации отпала.
* `os.Stat` + запрос в БД лежат внутри лока без нужды.

**Фикс:** как минимум вынести `os.Stat` и `MatchSessionPermission` **до**
взятия лока; правильнее — заменить единый `requestMu` на per-session
сериализацию (`csync.Map[sessionID]*sync.Mutex`) или убрать вовсе, оставив
`pendingRequests` (`csync.Map`) единственным механизмом — он уже полностью
конкурентно-безопасен, а `activeRequest` всё равно уже защищён отдельным
`activeRequestMu`.

---

### Medium

#### M-1. Дрейф `ForkSession` (сервер) vs `forkSessionCLI` (CLI) — оба теряют разные поля

**Файлы:** `internal/session/session.go:318-431` и
`internal/cmd/sessions_fork.go:106-207`

Это та самая точка дрейфа, помеченная в прошлом ревью как техдолг. Сейчас
можно назвать конкретику:

| Поле | `ForkSession` (web) | `forkSessionCLI` (CLI) |
|---|---|---|
| модели large/small | ✅ (`UpdateSessionModels`) | ✅ (через `CreateSessionParams`) |
| `system_prompt` | ✅ всегда | ✅ только если `!= ""` |
| `*_reasoning_effort` | ❌ **теряется** | ✅ |
| `todos` / `deleted_todos` | ✅ | ❌ **теряется** |
| `parent_session_id` | ❌ (по дизайну top-level) | ✅ `--child` |
| `--at N` усечение | ❌ | ✅ |
| pubsub `CreatedEvent` | ✅ | ❌ (обосновано: другой процесс) |
| дефолт заголовка | `"<title> fork"` | `"Fork of <title> (at msg N)"` |

То есть **кнопка «fork» в веб-UI молча теряет настройки reasoning effort**
исходной сессии, а `crush sessions fork` молча теряет todo-лист. Ни то, ни
другое не является намеренным решением — это следствие того, что две
реализации писались независимо.

**Фикс:** вынести одну транзакционную функцию в `internal/session`:

```go
type ForkOptions struct {
    NewID      string // "" → uuid
    Title      string // "" → "<src> fork"
    ParentID   string // "" → top-level
    LimitMsgs  int    // 0 → все
}
func (s *service) ForkSessionTx(ctx context.Context, srcID string, o ForkOptions) (Session, int, error)
```

`ForkSession` становится тонкой обёрткой с дефолтами, `sessions_fork.go`
вызывает её же через `a.Sessions`, а `db.Queries`-путь в `internal/cmd`
исчезает вместе с дублированием. Это заодно убирает необходимость держать в
`internal/cmd` знание о схеме таблицы `messages`.

---

#### M-2. `projects.Register` — неатомарный read-modify-write + неатомарная запись файла

**Файл:** `internal/projects/projects.go:35-117`

```go
func Register(workingDir, dataDir string) error {
    list, err := Load()      // берёт mu, ОТПУСКАЕТ mu
    ...                      // окно гонки
    return Save(list)        // снова берёт mu
}
```

`mu` отпускается между `Load` и `Save`, поэтому два конкурентных `Register`
(два `crush run` — а `projects.Register` вызывается на старте) теряют одну из
записей. Плюс:

* Никакого межпроцессного лока — ровно тот же класс бага, который только что
  закрыли для `ConfigStore` в `b53cbf3d`, но здесь он остался.
* `Save` (`:57-72`) пишет через `os.WriteFile` — **не атомарно**. Kill
  посередине оставляет усечённый/битый `projects.json`, и `Load` вернёт
  ошибку unmarshal для всех последующих запусков.

**Фикс:** переиспользовать уже существующие в проекте кирпичи —
`session.AcquireFileLockContext(ctx, path+".lock")` вокруг всего цикла
`Load→mutate→Save` и `fsext.AtomicWriteFile` вместо `os.WriteFile`
(`config.atomicWriteFile` делает то же для конфига). Реализация практически
копипаст из `ConfigStore.withConfigWriteLock`.

---

#### M-3. `maybeDelaySearch` спит до 2 секунд **под мьютексом** и не смотрит на context

**Файл:** `internal/agent/tools/search.go:203-218`

```go
func maybeDelaySearch() {
    lastSearchMu.Lock()
    defer lastSearchMu.Unlock()
    minGap := time.Duration(500+rand.IntN(1500)) * time.Millisecond
    if elapsed := time.Since(lastSearchTime); elapsed < minGap {
        time.Sleep(minGap - elapsed)   // сон ВНУТРИ критической секции
    }
    lastSearchTime = time.Now()
}
```

Два эффекта:
1. Классический «лок держится во время блокирующего ожидания». При N
   параллельных сессий, каждая делающая веб-поиск, задержки складываются
   последовательно: 10 сессий → до ~20 с суммарного ожидания у последней.
2. Сон не отменяем: отменённый агент (ctx cancelled, таймаут хода, юзер нажал
   cancel) всё равно досыпает до конца и держит лок.

**Фикс:** вычислить `wait` под локом, обновить `lastSearchTime` **сразу**
(зарезервировать слот), отпустить лок, а потом спать через
`select { case <-time.After(wait): case <-ctx.Done(): return ctx.Err() }`.
Функции нужно передать `ctx` (у вызывающего он есть).

---

#### M-4. `searchDuckDuckGo` — неограниченный `io.ReadAll` тела HTTP-ответа

**Файл:** `internal/agent/tools/search.go:72`

```go
body, err := io.ReadAll(resp.Body)   // без LimitReader
```

Все остальные HTTP-потребители в проекте это уже делают правильно:
`fetch.go:131` (`MaxFetchSize`), `fetch_helpers.go:47`, `sourcegraph.go:154`
(`maxSourcegraphBodyBytes`), `providers_models.go:113` (`10<<20`),
`oauth/hyper/device.go` (`1<<20`). Здесь — исключение. Скомпрометированный
или просто «залипший» эндпоинт (или редирект на большой файл) отъедает
неограниченную память в процессе агента.

**Фикс:** `io.ReadAll(io.LimitReader(resp.Body, maxSearchBodyBytes))` с
константой порядка 4–8 MiB. Заодно тот же паттерн стоит проверить в
`internal/oauth/copilot/oauth.go:167` и `internal/update/update.go:117`.

---

#### M-5. Единственное SQLite-соединение на процесс — жёсткая точка сериализации

**Файл:** `internal/db/connect.go:86` (`conn.SetMaxOpenConns(1)`)

Обоснование в комментарии верное (WAL/header desync → `SQLITE_NOTADB (26)` при
конкурентных писателях), и как защита от порчи БД это правильно. Но следствие:
**все** операции с БД в процессе — включая ~40 конкурентных WS-хендлеров
(**H-3**), heartbeat'ы локов, `sessions grep`, рекурсивные CTE по дереву
вызовов — стоят в одной очереди на одно соединение с `busy_timeout=30000`.
Один тяжёлый read (например `GetCallTreeActivityBatch` по большому дереву или
транскрипт-пагинация с глубоким OFFSET) блокирует запись сообщений агента.

Отдельно: `Connect` (`:70-105`) держит **глобальный** `poolMu` во время
`openDB` + `PingContext` + `goose.Up` (миграции). Открытие БД второго
workspace ждёт миграций первого. На практике редко, но это тоже «лок во время
I/O».

**Фикс (архитектурный, не срочный):** разделить пулы — один writer-коннект
(`SetMaxOpenConns(1)`, как сейчас) и отдельный read-only пул
(`file:...?mode=ro`, `_txlock=deferred`, N коннектов) для `List*`/`Get*`/
grep/CTE. WAL это штатно поддерживает: читатели не блокируют писателя.
Минимально — сузить `poolMu` до per-path блокировки.

---

#### M-6. Тесты `internal/config` сами воспроизводят тот баг, который «C» починил в `internal/cmd`

**Файлы:** `internal/config/load_test.go:103-104`,
`internal/config/store_test.go:329-330`, `:761-762`, `:814-815`,
`internal/config/store_snapshot_race_test.go:63-64`, `:230-231`, `:536-537`

Везде:

```go
t.Setenv("CRUSH_GLOBAL_CONFIG", dir)
t.Setenv("CRUSH_GLOBAL_DATA", dir)   // тот же dir!
```

Это ровно тот «latent duplicate-merge» (`lookupConfigs` грузит и мержит один и
тот же `<dir>/crush.json` дважды), который находка **C** в `054d23d6` устранила
в `models_use_test.go` и `providers_test.go`. Сейчас безвредно (сидируемые
конфиги не содержат массивов), но `LoadedPaths()` уже сейчас содержит
дублирующиеся записи, и первый же тест с массивным полем в конфиге начнёт
получать удвоенные элементы.

**Фикс:** те же два разных подкаталога, что и в `isolatedModelsEnv`. Заодно
имеет смысл вынести это в один хелпер (см. **H-1**) — сейчас идентичный
30-строчный комментарий скопирован в 4 файла, что само по себе сигнал.

---

#### M-7. `CheckOrigin: return true` на WebSocket-апгрейде

**Файл:** `internal/server/server.go:18-23`

```go
CheckOrigin: func(r *http.Request) bool { return true },
```

WS-хендшейк не подчиняется CORS. Единственное, что сейчас мешает произвольной
странице в браузере оператора открыть `ws://localhost:PORT/ws` и получить
полный контроль над агентом, — это `SameSite: Strict` на cookie
(`internal/server/auth.go:52`). Это работает в актуальных браузерах, но это
единственный слой: любое ослабление SameSite, любой браузер без поддержки, или
утечка токена в `?token=` (который принимается — `auth.go:95`) снимают защиту.
Классический CSWSH.

**Фикс:** реальная проверка Origin — принимать пустой Origin (не-браузерные
клиенты) и `http://localhost:<port>` / `http://127.0.0.1:<port>` / значение
из флага `--host`. Комментарий «tighten for production deployments» уже
намекает, что это было отложено; при наличии флага `-H 0.0.0.0` это уже не
теоретический сценарий.

---

### Low

#### L-1. `crush sessions fork` не может форкнуть пустую сессию

**Файл:** `internal/cmd/sessions_fork.go:126-132`

При `len(srcMsgs) == 0` и не заданном `--at`: `atN = 0` → срабатывает
`atN < 1` → ошибка `--at 0 is out of range (1..0)`. Форк только что созданной
сессии (легитимная операция; `ForkSession` её выполняет нормально) падает с
сообщением, которое ссылается на флаг, который пользователь не указывал.

**Фикс:** разрешить `len(srcMsgs) == 0` (создать пустой форк) и проверять
диапазон только когда `opts.atN != 0`.

#### L-2. Фантомное совпадение на пустой «строке» после финального `\n`

**Файл:** `internal/agent/tools/grep.go:530-552`

Если файл заканчивается `\n`, следующая итерация получает `io.EOF` с пустым
`lineBuf`, и `pattern.FindStringIndex("")` всё равно выполняется. Для паттерна,
матчащего пустую строку (`a*`, `^$`, `.*`), `onMatch` вызовется для
несуществующей строки `N+1`.

**Фикс:** `if rerr == io.EOF && lineBuf.Len() == 0 { break }` перед попыткой
матча.

#### L-3. `ListUserMessagesBySession` / `ListAllUserMessages` без tie-breaker'а

**Файл:** `internal/db/sql/messages.sql:66-74`

`ListMessagesBySession` и `ListMessagesBySessionPaginated` получили
`, rowid` в `95f4eb19`/`56f66cfd`, а эти два — нет, хотя проблема идентична
(`created_at` в секундах). Потребители — `message.ListUserMessages` /
`ListAllUserMessages` (`internal/message/message.go:290,305`), используются в
том числе для «последнего пользовательского сообщения». При совпадающем
`created_at` порядок не гарантирован.

**Фикс:** `ORDER BY created_at DESC, rowid DESC` в обоих + `sqlc generate`.

#### L-4. Ошибка чтения preview в HTTP-логгере не прерывает запрос

**Файл:** `internal/log/http.go:135-142`

Если `io.ReadAll(io.LimitReader(req.Body, ...))` вернул ошибку, она логируется,
но тело всё равно пересобирается из частичного `preview` + остатка того же
(уже сломанного) ридера. На практике запрос всё равно упадёт ниже по стеку, но
диагностика будет запутанной («сервер вернул 400» вместо «не смогли прочитать
тело»). Стоит вернуть ошибку сразу.

#### L-5. `copyFile` в `deploy.go` глотает ошибку `Close`

**Файл:** `deploy.go:419-434`

```go
defer out.Close()
...
return out.Sync()
```

`out.Sync()` покрывает большинство случаев, но ошибка `Close` (на некоторых
ФС/сетевых томах именно там всплывают отложенные ошибки записи) отбрасывается.
Для файла, который потом станет исполняемым бинарём, тихое усечение —
неприятный исход.

**Фикс:** явный `if err := out.Close(); err != nil { return err }` вместо
`defer` (или `defer` + именованный возврат).

#### L-6. `isContentionError` различает классы ошибок по подстроке

**Файл:** `internal/session/file_lock.go:141-148`

```go
return strings.Contains(err.Error(), "file lock contended")
```

Работает, потому что обе стороны строки — наш собственный код, но это хрупко:
рефакторинг текста в `TryAcquireFileLock` молча превратит «ждать и
ретраить» в «упасть сразу», и ни один тест это не поймает по текущему
контракту.

**Фикс:** типизированная ошибка (`type ErrLockContended struct{ Path string }`)
+ `errors.As`. Дёшево и однозначно.

#### L-7. `s.hub.unregister <- c` может заблокировать `readPump` навсегда

**Файл:** `internal/server/server.go:128-131`

После завершения `Hub.Run` (ctx отменён) никто не читает из `unregister`
(буфер 64). 65-й отключающийся клиент повиснет на отправке, и его
`c.conn.Close()` (отложенный после) не выполнится — утечка горутины +
соединения на шатдауне.

**Фикс:** `select { case s.hub.unregister <- c: case <-ctx.Done(): }` и
безусловный `c.conn.Close()` в отдельном `defer`.

#### L-8. `persistentMode` пишется и читается без синхронизации

**Файлы:** `internal/agent/coordinator.go:428` (запись), `:474` (чтение)

`SetPersistentMode` — обычное присваивание `bool`. Фактически вызывается один
раз на старте, до появления конкурентных читателей, поэтому гонка сейчас
недостижима — но это единственное поле в этом блоке без `atomic`/мьютекса,
тогда как соседние (`allowPeakHours`, `activeModelRole`, `maxCost`) защищены.
`atomic.Bool` стоит ноль и снимает вопрос.

---

### Nit

* **N-1.** `internal/server/hub.go:165` — возвращаемое `push` значение
  игнорируется; либо использовать (метрика «сколько событий не попало в
  replay»), либо сделать `push` без возврата.
* **N-2.** `newReplayBuffer` всегда аллоцирует 2000 указателей (~16 KiB) на хаб
  сразу. Для одного хаба несущественно; упомянуто только чтобы это было
  осознанным выбором, а не сюрпризом, если хабы когда-нибудь станут
  per-session.
* **N-3.** `internal/server/auth.go:88-96` — сравнение токена через `==`
  (не constant-time). Для 160-битного токена по локальному loopback риск
  практически нулевой, но `subtle.ConstantTimeCompare` стоит одну строку и
  снимает вопрос при `-H 0.0.0.0`.
* **N-4.** `internal/csync/maps.go:80` `GetOrSet` — не атомарен
  (`Get` → `fn()` → `Set`), два вызова могут оба выполнить `fn`. Оба текущих
  вызывающих (`internal/fsext/ls.go:170,194`) идемпотентны, так что баг
  недостижим — но имя обещает атомарность, которой нет. Либо переименовать,
  либо реализовать под одним `Lock`.
* **N-5.** `internal/config/docker_mcp.go:55-62` —
  `RefreshDockerMCPAvailability` не имеет single-flight: N конкурентных
  вызовов запустят N подпроцессов `docker mcp version`. `golang.org/x/sync/
  singleflight` или простой «идёт проверка» флаг.
* **N-6.** `internal/deploy/deploy.go:116,127` — `filepath.Glob(dst + ".old-*")`
  сломается, если путь установки содержит метасимволы glob (`[`, `?`).
  На Windows-путях вида `%LOCALAPPDATA%\Programs\crush` невозможно, но
  `filepath.Glob` не даёт способа заэкранировать — при желании заменить на
  `os.ReadDir` + `strings.HasPrefix`.
* **N-7.** `internal/shell/dispatch.go:363-377` — `wslLauncherPaths()` не
  покрывает `%SystemRoot%\Sysnative\{bash,wsl}.exe` (путь, который видит
  32-битный процесс). Форк собирается 64-битным, так что сейчас недостижимо.
* **N-8.** `internal/shell/background.go:69` — `bufferRetention` это
  package-level `var`, который переопределяют тесты; если такой тест когда-
  нибудь получит `t.Parallel()`, будет гонка. Стоит зафиксировать
  комментарием (как это уже сделано для `TestIsWSLLauncher_FallsBackWhenSystemRootUnset`).
* **N-9.** `internal/config/load.go:317` — `store.RemoveConfigField(...)`
  вызывается без обработки возвращаемой ошибки и без `_ =`; комментарий рядом
  объясняет поведение, но не то, что ошибка намеренно игнорируется.
* **N-10.** `RemoveConfigField` создаёт `<dir>/crush.json.lock` (и родительский
  каталог) даже когда самого `crush.json` не существует — на чистой машине это
  оставляет пустой сайдкар. Безвредно, но неопрятно; можно проверять
  существование файла до взятия лока.

---

## 3. Что сделано хорошо

Коротко, чтобы не разбавлять находки:

* **Комментарии-обоснования на уровне, который редко встречается.** Практически
  каждое неочевидное решение (почему `.lock` не удаляется; почему нет
  per-message pubsub в форке; почему `NULL` vs `""` в `summary_message_id`;
  почему rowid, а не `id`; почему отклонён keyset-пагинатор) объяснено там же,
  в коде, вместе с тем, что нужно проверить перед «починкой». Это резко
  снижает шанс регрессии от следующего ревьюера.
* **Проверка того, что баг реален, а не гипотетичен.** Тесты вида
  `TestUnprotectedRMW_DeterministicallyLosesUpdate` и
  `TestBroadOverwriteLosesConcurrentUsage`, которые воспроизводят **старое**
  поведение и доказывают потерю данных — правильный способ обосновать фикс.
* **Замена «широкой» записи на column-scoped UPDATE** (`524fbe58`) с явным
  удалением `Save` из интерфейса, чтобы регрессию нельзя было внести заново, —
  это лечение класса, а не симптома.
* **`replayBuffer`** — аккуратная, полностью O(1) реализация с тремя
  независимыми границами и тестом на нулевые аллокации при эвикции.
* **Лок-дисциплина в `ConfigStore`**: `publishMu` как единая точка публикации
  снапшота, copy-on-write, `TryLock` в `autoReload` вместо реэнтрантности,
  «единственная мутация — `snap.Store` в самом конце» как способ получить
  бесплатный rollback. Это хороший дизайн.
* **`SessionLock`**: решение «реклейм только через реальный OS-лок, mtime —
  исключительно диагностика» с описанием конкретного бага, который к этому
  привёл, — образцовое.
* **Дисциплина изоляции тестов** в `isolatedModelsEnv` (после `054d23d6`) —
  включая учёт двух разных путей конфига и запрет на `t.Parallel()` там, где
  используется сырой `os.Unsetenv`.
* Проект проходит `go vet` и `golangci-lint` (v2.10) **с нулём замечаний**, а
  все concurrency-тяжёлые пакеты — `-race` чисто.

---

## 4. Рекомендуемый порядок работ

1. **H-1** — изоляция `CRUSH_GLOBAL_CONFIG` в `internal/agent/coordinator_test.go`
   (4 места). Самое дешёвое из High и напрямую продолжает только что
   закрытую тему; пока не сделано, любой прогон тестов на машине оператора
   читает его боевой конфиг. Сразу же **M-6** — тот же хелпер, соседние файлы.
2. **H-2** — сократить/обойти 30-секундное ожидание файлового лока под
   `publishMu`. Это регрессия, внесённая в этом же раунде; чинить пока
   контекст свежий.
3. **H-3** — `recover()` вокруг WS-хендлеров. Одна обёртка, ~15 строк,
   убирает целый класс «весь сервер упал».
4. **M-1** — единая транзакционная `ForkSessionTx`. Устраняет реальную
   потерю данных в обоих направлениях (reasoning effort в web, todos в CLI) и
   закрывает известную точку дрейфа насовсем. Заодно попадает **L-1**.
5. **H-4** — вынести `os.Stat`/`MatchSessionPermission` из-под `requestMu` и
   перевести сериализацию на per-session. Требует чуть больше размышлений о
   контракте веб-UI, поэтому после пунктов 1–4.
6. **M-2** — атомарность `projects.Register`/`Save`. Механически: копия уже
   написанного `withConfigWriteLock` + `fsext.AtomicWriteFile`.
7. **M-3**, **M-4** — обе в `internal/agent/tools/search.go`, чинятся одним
   заходом (~20 строк).
8. **M-7** — реальная проверка Origin на WS.
9. **L-2**, **L-3**, **L-5**, **L-6**, **L-7** — мелкие, независимые, можно
   раздать пачкой.
10. **M-5** — разделение read/write пулов SQLite. Самая крупная по объёму
    работа и единственная, требующая отдельного проектирования и нагрузочной
    проверки; логично делать после того, как **H-3** ограничит фан-аут
    горутин, иначе эффект будет сложно измерить.
11. **N-*** — по остаточному принципу, удобно закрыть вместе с любой соседней
    правкой в том же файле.
