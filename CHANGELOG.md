# Changelog

## Unreleased

### Fixed

- On macOS a profile's CPU time goes to the code that used it. The
  kernel there hands the profiler's signals mostly to a thread entering
  it (a query's socket), and one signal for several milliseconds to a
  thread that computes, so a CPU-bound loop beside queries got about
  half its time and the code that queried most of the rest. A sample
  taken at a system call now stands for one millisecond, and the
  process's other CPU time is shared among the other samples. It is an
  estimate, and the profile says so (`estimated_from`: the samples
  taken); the console shows it as one. Linux takes every sample, one a
  millisecond, and needs none.

## 0.10.3 (2026-11-01)

### Changed

- Runs `tracepanel-agent` 0.10.3: waiting has one name and look in a
  profile, and the framework and waiting rows sit below your functions.

## 0.10.2 (2026-11-01)

### Changed

- Runs `tracepanel-agent` 0.10.2: a profile's Your code holds only the app's
  frames, and the console says how much of the trace was on the CPU, with
  the rest shown as waiting.

## 0.10.1 (2026-11-01)

### Changed

- A profile samples every millisecond of CPU time, as the Python and
  JavaScript SDKs do, not every 10 ms (Go's own rate): a request of a
  hundred milliseconds has a hundred samples, not ten, and the console
  says "one every 1 ms". `TRACEPANEL_PROFILE_INTERVAL_MS` (or
  `Config.ProfileInterval`) sets another rate. The runtime's warning that
  the rate was set before `runtime/pprof` started never reaches stderr.
  A profiled request on a CPU-bound handler costs about 4% more CPU than
  at Go's rate.

### Fixed

- gormtrace: `Scan` into one value through a pointer
  (`var avg *float64; db.Select("AVG(rating)").Scan(&avg)`) or into a
  named type said it returned 0 rows, as GORM counts it; it says 1.
- gormtrace: a database whose `gorm.Config.Logger` is nil gets its rows
  counted (and `Scan` no longer panics there), as with `logger.Discard`
  or any other logger.

## 0.10.0 (2026-10-31)

Downloads `tracepanel-agent` 0.10.0 the first time. The first release of
Tracepanel for Go, with the agent and the other SDKs at 0.10. Tags are
`v0.10.0`, as Go modules name their versions.

Records only in development, as the other SDKs do: under `go run`, with
`TRACEPANEL_ENABLED=1` or with `Config.Enabled`. A binary `go build`
wrote records nothing, and its `WrapHandler`, `Driver` and the rest
hand back what they were given; `TRACEPANEL_ENABLED=0` always wins.

### Added

- `tracepanel.WrapHandler(mux)`, or `tracepanel.Middleware` for another
  router: each request a trace, named by its `ServeMux` route (or
  `Config.Route`), its handler a `view` span, a panic with its stack and
  source, the badge's headers on every response and the badge on HTML
  pages. Only a page is held for the badge: any other response (JSON, a
  file, a stream) goes out as it is written, and `http.ResponseController`
  reaches the connection.
- `sql.Open(tracepanel.Driver("pgx"), dsn)`, and
  `tracepanel.WrapConnector` for `sql.OpenDB`: each statement with its
  parameters, the rows it returned and the line that ran it, and each
  transaction, so the agent finds N+1 queries, duplicates and slow
  SQL. `EXPLAIN` of a recorded query from the agent's console, run by the
  app on its database and rolled back.
- The SQL rules of every SDK: a statement on one line, a line comment
  (`-- a note`) closed as `/* a note */`; its name and fingerprint without
  comments (a sqlcommenter traceparent differs in every query) and its
  `IN` list of placeholders one `IN (...)`, whatever the driver writes
  (`?`, `$1`, `@p1`, `:name`); its name the table without its schema or
  quotes (`SELECT orders`), and `SELECT COUNT orders` for a count.
- `tracepanel.TxContext(ctx, tx)`: the work done with it while the
  transaction is open (an HTTP call, a log record, a span) goes under the
  transaction's span, so the agent finds an HTTP call inside a
  transaction.
- `slog` records logged with a request's context, with the line that
  logged them, secret-looking fields masked, and calls through
  `http.DefaultClient` or `tracepanel.WrapTransport` with the request's
  context, with a `traceparent`,
  and `X-Tracepanel-Profile` when the request was asked to be profiled, so
  the apps down the chain profile their part.
- `tracepanel.Span` for the app's own code, `tracepanel.Go` for a task
  linked to the request that started it, `tracepanel.Command` for a job
  or a command, and `tracepanel.SetUser(ctx, id, username)` for the
  signed-in user (the username stays on this machine).
- Profiles: the panel's Profile toggle (or `X-Tracepanel-Profile: 1`)
  records a request with a flame graph of where its CPU time went, from
  `runtime/pprof`, its goroutines labeled so that only its own samples
  are its, with any number of requests at once; a task it starts with
  `tracepanel.Go` is profiled too, and `TRACEPANEL_PROFILE=1` (or
  `Config.Profile`) profiles every trace, and `tracepanel.Profile(ctx)`
  the rest of the trace from where the code asks; a profiled trace says
  why in `profile.reason` (`header`, `cookie`, `parent`, `setting`,
  `code`): `setting` when the setting would profile it anyway, else the
  first ask that profiled it, as in the other SDKs. One CPU profile runs
  on from
  one profiled trace to the next, cut once none ran for 200 ms (every 2 s
  under steady load), so back-to-back traces lose no samples to the
  profiler's restart (up to 100 ms on macOS); a trace running while a cut
  stops it says for how long (`tracepanel.profile_gap_ms`). Samples the
  profiler lost are counted as `dropped`. Every profile has the garbage
  collector's runs and their pauses (`gc`). Asked for `memory` (or
  `TRACEPANEL_PROFILE_MEMORY=1`), its `memory` too: the bytes the heap
  allocated while the trace ran, its peak above the start and its net
  change, from `runtime/metrics`. Go's heap can't be told per goroutine,
  so these are the process's while the trace ran, marked `shared` when
  another trace ran meanwhile, without allocation sites.
- `tracepanel.Dump(ctx, values...)`, a debug print: each value with its
  expression, as Go syntax, its type and its length on stderr, and in a
  request's trace a dump in the call tree, secret-named values masked,
  inside a value too: a struct's field or a map's key named as a secret.
- The console's Request tab: a request's headers, query, cookies, the
  body as far as the handler read it (16 KB; JSON and forms masked,
  binary left out) and the response's headers, secret-looking values
  masked; `TRACEPANEL_CAPTURE_REQUEST=0` keeps none of it.
- One secret rule for every value masked by its name (a request's
  headers, query, cookies and body, a log record's fields, a dump's
  expressions, fields and keys), the other SDKs': a name read word by
  word (camelCase, snake_case, kebab-case, dots), so `authToken`,
  `AUTH_HEADER` and `X-Api-Key` are masked and `Author`, `tokenizer`,
  `token_type` and `hasPassword` aren't; `tokens` holding numbers is a
  model's counts, shown.
- The integrations, each a module of its own tagged with the core
  (`gintrace/v0.10.0`), so the core keeps no dependencies:
  - `gintrace.Middleware()` for gin, `echotrace.Middleware()` for Echo v4
    and `echo5trace.Middleware()` for Echo v5: each request traced as
    `WrapHandler` traces it, named by its route (`GET /books/:id`), its
    handler a `view` span (not on Echo v5, which doesn't tell it), an
    error a handler returns answered by Echo's error handler inside the
    trace, and the badge naming the router (`Go 1.27 · gin`).
  - `pgxtrace.Tracer()`, pgx v5's tracer on a `pgx.Conn` or a
    `pgxpool.Pool`: each statement, batch, copy and transaction as
    `tracepanel.Driver` records them, `EXPLAIN` from the console, and
    `pgxtrace.TxContext` for the rest of a transaction's work.
  - `db.Use(gormtrace.Plugin())` for GORM v2: each statement of a create,
    query, update, delete or raw SQL with its parameters, the rows it
    returned or changed (`Scan`'s too, from what GORM tells its logger;
    not `Rows()`'s, which the app reads itself: `tracepanel.Driver`
    counts those) and the line that ran it, each transaction
    (GORM's own around a write too), `EXPLAIN` from the console, and
    `gormtrace.TxContext` for the rest of a transaction's work. A
    database GORM opened through `tracepanel.Driver` is left to it.
  - `redistrace.Instrument(rdb)`, go-redis v9: each command a span with
    its key, the database and the server, a pipeline one span, so the
    agent finds a command repeated for each item and duplicates.
  - `zaptrace.WrapCore` with a `zaptrace.Context(ctx)` field, and
    zerolog's `zerologtrace.Writer` and `zerologtrace.Hook`: their records
    join the trace of the context they name, as `slog`'s do, fields
    masked the same way.
  - `grpctrace.ServerOptions()` and `grpctrace.DialOptions()`: each call
    served an `rpc` trace named `Service/Method`, each call made a span,
    a `traceparent` in the metadata both ways. A call with the
    `x-tracepanel-profile` metadata key (`1`, as the HTTP header) has its
    trace profiled, and a call made in a profiled trace asks the same.
  - `kafkatrace.WrapWriter` for segmentio/kafka-go: the messages sent a
    `publish` span, each carrying a `traceparent`; `kafkatrace.Process`
    makes each message handled a task trace linked to the publish that
    sent it, in another app too. A message sent in a profiled trace
    carries `tracepanel-profile`, as the other SDKs' messages do, and its
    handler's trace is profiled too.
- The console's App tab: Go, the router, the app's module and the modules
  it depends on, and the build's settings that hold no secret.
- `TRACEPANEL_N_PLUS_ONE_MIN_MS`, `TRACEPANEL_QUEUE_WAIT_MS` and
  `TRACEPANEL_MAX_TRACES`, and a limit per database in
  `TRACEPANEL_SLOW_SQL_MS` and `TRACEPANEL_LONG_TRANSACTION_MS`:
  `100,snowflake=3000`, the database's kind told by its driver
  (gosnowflake's is `snowflake`, BigQuery's, Databricks', Redshift's
  too), else by `tracepanel.DBSystem`.
- The `tracepanel` command (`go install
  github.com/tracepanel/tracepanel-go/cmd/tracepanel@v0.10.0`):
  `tracepanel mcp`, the agent's MCP server for AI coding agents, `dev`
  for its console in the foreground (`dev -- ./shop` with the app beside
  it; `dev --port 9100 -- ./shop` too), and `run ./shop` (or
  `run -- ./shop`) for a built binary, recording, which exits with
  the app's code and says on stderr how an app that failed ended
  (`tracepanel: ./shop exited with status 3`), the app's status under
  `go run` too, which exits 1 whatever the code.
- The agent downloaded the first time from its npm platform package (or
  `TRACEPANEL_NPM_REGISTRY`, a mirror), checked against the SHA-512 of its
  binary the SDK was released with, and kept in the user's cache, where
  it is checked again the first time a process runs it: in
  `TRACEPANEL_CACHE_DIR`, else `%LOCALAPPDATA%\tracepanel` on Windows,
  else `$XDG_CACHE_HOME/tracepanel`, else `~/.cache/tracepanel` (macOS
  too).
- A trace keeps at most 2,000 spans and 1,000 log records, as the agent
  stores it, and counts the rest.
- Linux, macOS and Windows: CI runs the tests and an app against the real
  agent on Windows too (`go run`'s binaries in `%TEMP%\go-build…` and the
  build cache, the agent's `.exe` downloaded into `%LOCALAPPDATA%` and
  started in a process group of its own), and builds every module for
  Windows on amd64 and arm64; a release waits for Windows' jobs.

The SDK records on its own and has no dependencies: no OpenTelemetry. An
integration depends on its library only.
