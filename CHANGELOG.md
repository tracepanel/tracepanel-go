# Changelog

## 0.10.0 (Unreleased)

Downloads `tracepanel-agent` 0.10.0 the first time. The first release of
Tracepanel for Go, with the agent and the other SDKs at 0.10. Tags are
`v0.10.0`, as Go modules name their versions.

### Added

- `tracepanel.WrapHandler(mux)`, or `tracepanel.Middleware` for another
  router: each request a trace, named by its `ServeMux` route (or
  `Config.Route`), its handler a `view` span, a panic with its stack and
  source, the badge's headers on every response and the badge on HTML
  pages. Only a page is held for the badge: any other response (JSON, a
  file, a stream) goes out as it is written, and `http.ResponseController`
  reaches the connection.
- `sql.Open(tracepanel.WrapDriver("pgx"), dsn)`, and
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
  `http.DefaultClient` or `tracepanel.WrapTransport` with a `traceparent`,
  and `X-Tracepanel-Profile` when the request was asked to be profiled, so
  the apps down the chain profile their part.
- `tracepanel.Span` for the app's own code, `tracepanel.Go` for a task
  linked to the request that started it, `tracepanel.Command` for a job
  or a command, and `tracepanel.SetUser(ctx, id, username)` for the
  signed-in user (the username stays on this machine).
- The console's App tab: Go, the router, the app's module and the modules
  it depends on, and the build's settings that hold no secret.
- `TRACEPANEL_N_PLUS_ONE_MIN_MS`, `TRACEPANEL_QUEUE_WAIT_MS` and
  `TRACEPANEL_MAX_TRACES`, and a limit per database in
  `TRACEPANEL_SLOW_SQL_MS` and `TRACEPANEL_LONG_TRANSACTION_MS`:
  `100,snowflake=3000`.
- `go run github.com/tracepanel/tracepanel-go/cmd/tracepanel mcp`: the
  agent's MCP server for AI coding agents, and `dev` for its console in
  the foreground.
- The agent downloaded the first time from its npm platform package (or
  `TRACEPANEL_NPM_REGISTRY`, a mirror), checked against the SHA-512 of its
  binary the SDK was released with, and kept in the user's cache, where
  it is checked again the first time a process runs it.
- A trace keeps at most 2,000 spans and 1,000 log records, as the agent
  stores it, and counts the rest.

The SDK records on its own and has no dependencies: no OpenTelemetry.
