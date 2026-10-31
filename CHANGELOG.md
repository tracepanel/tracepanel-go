# Changelog

## 0.10.0 (Unreleased)

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
  `Config.Profile`) profiles every trace. Samples the profiler lost are
  counted as `dropped`. Not memory: Go's heap profile is the process's.
- The console's Request tab: a request's headers, query, cookies, the
  body as far as the handler read it (16 KB; JSON and forms masked,
  binary left out) and the response's headers, secret-looking values
  masked; `TRACEPANEL_CAPTURE_REQUEST=0` keeps none of it.
- The integrations, each a module of its own tagged with the core
  (`gintrace/v0.10.0`), so the core keeps no dependencies:
  - `gintrace.Middleware()` for gin, `echotrace.Middleware()` for Echo v4
    and `echo5trace.Middleware()` for Echo v5: each request traced as
    `WrapHandler` traces it, named by its route (`GET /books/:id`), its
    handler a `view` span (not on Echo v5, which doesn't tell it), an
    error a handler returns answered by Echo's error handler inside the
    trace, and the badge naming the router (`Go 1.27 · gin`).
  - `pgxtrace.Tracer()`, pgx v5's tracer on a `pgx.Conn` or a `pgxpool.Pool`:
    each statement, batch, copy and transaction as `tracepanel.Driver` records
    them, `EXPLAIN` from the console, and `pgxtrace.TxContext` for the
    rest of a transaction's work.
  - `redistrace.Instrument(rdb)`, go-redis v9: each command a span with
    its key, the database and the server, a pipeline one span, so the
    agent finds a command repeated for each item and duplicates.
  - `zaptrace.WrapCore` with a `zaptrace.Context(ctx)` field, and
    zerolog's `zerologtrace.Writer` and `zerologtrace.Hook`: their records
    join the trace of the context they name, as `slog`'s do, fields
    masked the same way.
  - `grpctrace.ServerOptions()` and `grpctrace.DialOptions()`: each call
    served an `rpc` trace named `Service/Method`, each call made a span,
    a `traceparent` in the metadata both ways.
  - `kafkatrace.WrapWriter` for segmentio/kafka-go: the messages sent a
    `publish` span, each carrying a `traceparent`; `kafkatrace.Process`
    makes each message handled a task trace linked to the publish that
    sent it, in another app too.
- The console's App tab: Go, the router, the app's module and the modules
  it depends on, and the build's settings that hold no secret.
- `TRACEPANEL_N_PLUS_ONE_MIN_MS`, `TRACEPANEL_QUEUE_WAIT_MS` and
  `TRACEPANEL_MAX_TRACES`, and a limit per database in
  `TRACEPANEL_SLOW_SQL_MS` and `TRACEPANEL_LONG_TRANSACTION_MS`:
  `100,snowflake=3000`, the database's kind told by its driver
  (gosnowflake's is `snowflake`, BigQuery's, Databricks', Redshift's
  too), else by `tracepanel.DBSystem`.
- `go run github.com/tracepanel/tracepanel-go/cmd/tracepanel mcp`: the
  agent's MCP server for AI coding agents, `dev` for its console in the
  foreground (`dev -- ./shop` with the app beside it), and `run ./shop`
  for a built binary, recording.
- The agent downloaded the first time from its npm platform package (or
  `TRACEPANEL_NPM_REGISTRY`, a mirror), checked against the SHA-512 of its
  binary the SDK was released with, and kept in the user's cache, where
  it is checked again the first time a process runs it.
- A trace keeps at most 2,000 spans and 1,000 log records, as the agent
  stores it, and counts the rest.

The SDK records on its own and has no dependencies: no OpenTelemetry. An
integration depends on its library only.
