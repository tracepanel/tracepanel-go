# Tracepanel for Go

Tracepanel shows what each request of your app did: its SQL with the line of
your code that ran each statement, its logs, its outgoing calls and its
panics. It finds what to fix: N+1 queries, duplicates, slow SQL, long
transactions. A badge on your pages opens the panel, and the console lists
every request.

This SDK records a Go app's requests and sends them to Tracepanel's agent,
which keeps them, finds the issues and serves the console. It has no
dependencies.

## Setup

```bash
go get github.com/tracepanel/tracepanel-go
```

Open the database with `tracepanel.Open` in place of `sql.Open`, and wrap
the app's handler:

```go
import tracepanel "github.com/tracepanel/tracepanel-go"

db, err := tracepanel.Open("pgx", dsn)
// ...
http.ListenAndServe(addr, tracepanel.Wrap(mux))
```

Run the app as you do (`go run .`). The first request starts the agent for
the project, and the response tells you where its console is: the badge on
an HTML page opens it, and `Tracepanel-Url` names it.

The agent is a program of its own, which keeps the traces and finds the
issues. You don't install it: the SDK downloads its own version of it the
first time, from the agent's npm package for your platform, checks it
against the checksum it was released with, and keeps it in
`~/.cache/tracepanel/` for every project. `TRACEPANEL_AGENT_BIN` names
another one, and without a network the SDK runs `tracepanel-agent` from the
`PATH`.

Go 1.26 or later.

## What it records

- Each request, named by its route (`GET /books/{id}`, the `ServeMux`
  pattern), with the handler that served it as a `view` span.
- SQL through `database/sql`, whatever runs it (your own queries, sqlx,
  sqlc, GORM or Bun on the `*sql.DB`): each statement with its parameters,
  the rows it returned and the line of your code that ran it, and each
  transaction around its statements.
- `slog` records logged with the request's context
  (`slog.InfoContext(ctx, ...)`), with the line that logged them.
- Calls to HTTP APIs through `http.DefaultClient` (`http.Get` too) or a
  client with `tracepanel.Transport(base)`, each with a `traceparent`, so
  the app it reaches links its trace to the call.
- Panics, with your frames and the source around them. The request
  answers 500.
- The caller's trace, when a request carries a W3C `traceparent`.

Every response carries `Tracepanel-Id` and `Server-Timing`, and an HTML page
gets the badge. A response the handler flushes (a stream) or takes over (a
WebSocket) goes out as it is written, without the badge.

A statement, a log record or a call joins the request's trace through its
context: pass `r.Context()` down, as in `db.QueryContext(ctx, ...)`. Work
without it isn't recorded.

### Other routers

`ServeMux` names each route. For another router, `Config.Route` returns it:

```go
tracepanel.Wrap(router, tracepanel.Config{
	Route: func(r *http.Request) string { return chi.RouteContext(r.Context()).RoutePattern() },
})
```

### Databases

`tracepanel.OpenDB` takes a driver's connector, as `sql.OpenDB` does. The
console runs `EXPLAIN` of a recorded `SELECT` on the database it came from.
With a second database, name it:

```go
replica, err := tracepanel.Open("pgx", replicaDSN, tracepanel.Alias("replica"))
```

## Your own code, tasks and commands

```go
// A span of the request's trace, until end().
ctx, end := tracepanel.Span(ctx, "render invoice")
defer end()

// A task: a trace of its own on a goroutine, linked to the request that started it.
tracepanel.Go(ctx, "send welcome email", func(ctx context.Context) error {
	return mailer.Send(ctx, user)
})

// A command or a scheduled job: a trace of its own.
err := tracepanel.Command(ctx, "nightly report", report)
```

A program without an HTTP server (a worker, a command line tool) calls
`tracepanel.Setup()` first, and `tracepanel.Flush(time.Second)` before it
exits, so its traces reach the agent.

## Settings

`tracepanel.Wrap(mux, tracepanel.Config{...})` and `tracepanel.Setup(...)`
take a `Config`. What it leaves empty comes from the environment:

| Field | Variable | Default |
| --- | --- | --- |
| | `TRACEPANEL_ENABLED` | on; `0` turns Tracepanel off |
| `Service` | `TRACEPANEL_SERVICE` | the executable's name |
| `StoreDir` | `TRACEPANEL_DIR` | `.tracepanel`, the project's store |
| `ConsoleURL` | `TRACEPANEL_CONSOLE_URL` | where the project's agent serves it |
| `MaxTraces` | | `2000`, the traces the agent keeps |
| | `TRACEPANEL_AGENT_BIN` | the one the SDK downloads |
| | `TRACEPANEL_AGENT` | `auto`; an agent elsewhere (`tcp:host:port`), never started; `off` starts none |
| | `TRACEPANEL_CACHE_DIR` | `~/.cache/tracepanel`, where the downloaded agent is kept |
| | `TRACEPANEL_N_PLUS_ONE_THRESHOLD` | `3`, repeats that make an N+1 |
| | `TRACEPANEL_SLOW_SQL_MS` | `100`, a slow query |
| | `TRACEPANEL_LONG_TRANSACTION_MS` | `500`, a long transaction |
| | `TRACEPANEL_SLOW_HTTP_MS` | a slow outgoing call |
| | `TRACEPANEL_LARGE_RESULT_ROWS` | `1000`, a query without a LIMIT that read too many rows |

Without an agent (none could be had, or it couldn't start), the log says
why once. Responses keep their headers, pages get no badge, and traces
aren't kept.

For an AI coding agent (Claude Code, Cursor, Codex), the same traces over
MCP, from the project's directory:

```bash
claude mcp add --scope project tracepanel -- tracepanel-agent mcp
```

## Development

```bash
make lint    # gofmt, go vet, the tests
```

`go generate` writes `integrity.go`, the checksums of the agent's downloads,
after `AgentVersion` changes in `version.go`. The test that starts the
agent runs with a build of the sibling `tracepanel` checkout, or the one
`TRACEPANEL_AGENT_BIN` names.

## License

MIT
