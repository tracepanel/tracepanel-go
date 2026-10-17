# Changelog

## Unreleased

### Added

- `tracepanel.TxContext(ctx, tx)`: the work done with it while the
  transaction is open (an HTTP call, a log record, a span) goes under the
  transaction's span, so the agent finds an HTTP call inside a
  transaction. Once the transaction ends, it goes under the request again.

## 0.3.0 (2026-10-15)

The first release, with `tracepanel-agent` 0.3.0, which the SDK downloads
the first time. Tags are `v0.3.0`, as Go modules name their versions.

### Added

- `tracepanel.WrapHandler(mux)`, or `tracepanel.Middleware` for another
  router: each request a trace, named by its `ServeMux` route (or
  `Config.Route`), its handler a `view` span, a panic with its stack and
  source, the badge's headers on every response and the badge on HTML
  pages. A stream or a WebSocket goes out as it is written.
- `sql.Open(tracepanel.WrapDriver("pgx"), dsn)`, and
  `tracepanel.WrapConnector` for `sql.OpenDB`: each statement with its
  parameters, the rows it returned and the line that ran it, and each
  transaction, so the agent finds N+1 queries, duplicates and slow
  SQL. `EXPLAIN` of a recorded query from the agent's console, run by the
  app on its database and rolled back.
- `slog` records logged with a request's context, with the line that
  logged them, and calls through `http.DefaultClient` or
  `tracepanel.WrapTransport` with a `traceparent`.
- `tracepanel.Span` for the app's own code, `tracepanel.Go` for a task
  linked to the request that started it, and `tracepanel.Command` for a
  job or a command.
- The agent downloaded the first time from its npm platform package,
  checked against the checksum the SDK was released with, and kept in the
  user's cache.
- A trace keeps at most 2,000 spans and 1,000 log records, as the agent
  stores it, and counts the rest.

### Removed

- OpenTelemetry: the SDK records on its own and has no dependencies.
  `otelsql.Open("pgx", dsn)` becomes
  `sql.Open(tracepanel.WrapDriver("pgx"), dsn)`.
