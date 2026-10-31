# Tracepanel for Go

Tracepanel shows what each request of your app did: its SQL with the line of
your code that ran each statement, its logs, its outgoing calls and its
panics. It finds what to fix: N+1 queries, duplicates, slow SQL, long
transactions. A badge on your pages opens the panel, and the console lists
every request.

This SDK records a Go app's requests and sends them to Tracepanel's agent,
which keeps them, finds the issues and serves the console. It has no
dependencies: the integrations with gin, Echo, pgx, GORM, go-redis, zap,
zerolog, gRPC and Kafka are modules of their own, and you get only those you
use.

## Setup

```bash
go get github.com/tracepanel/tracepanel-go
```

Give `sql.Open` the driver's name through `tracepanel.Driver`, and wrap
the app's handler:

```go
import tracepanel "github.com/tracepanel/tracepanel-go"

db, err := sql.Open(tracepanel.Driver("pgx"), dsn)
// ...
http.ListenAndServe(addr, tracepanel.WrapHandler(mux))
```

Run the app with `go run .`. The first request starts the agent for the
project, and the response tells you where its console is: the badge on an
HTML page opens it, and `Tracepanel-Url` names it.

That is all a net/http app with database/sql needs. The calls it makes
through `http.DefaultClient` with the request's context, and what it logs
through `slog`, are recorded too. Other routers, pgx, GORM, Redis, zap,
zerolog, gRPC and Kafka take a line each: see
[What it records](#what-it-records).

### Development only

Tracepanel records in development only, as every Tracepanel SDK does:
when the app runs under `go run`, with `TRACEPANEL_ENABLED=1`, or with
`Config.Enabled`. A binary built with `go build` (your production build,
a container's) records nothing: `WrapHandler`, `Driver` and the rest
hand back what they were given, and no agent is downloaded or started.
`TRACEPANEL_ENABLED=0` turns it off whatever else says. To record a built
binary, run it through the SDK's command, which sets
`TRACEPANEL_ENABLED=1` for it:

```bash
go install github.com/tracepanel/tracepanel-go/cmd/tracepanel@v0.10.0
tracepanel run ./shop
```

`tracepanel run` exits with the app's exit code, and says on stderr how
an app that failed ended (`tracepanel: ./shop exited with status 3`).
`go run github.com/tracepanel/tracepanel-go/cmd/tracepanel@v0.10.0 run
./shop` works without installing it, but `go run` exits 1 whatever the
program's code, so that line is where the app's status is.

### The agent

The agent is a program of its own, which keeps the traces and finds the
issues. You don't install it: the SDK downloads its own version of it the
first time, from the agent's npm package for your platform, checks the
binary against the SHA-512 the SDK was released with, and keeps it in
the user's cache for every project, where it is checked again the first
time a process runs it: `TRACEPANEL_CACHE_DIR`, else
`%LOCALAPPDATA%\tracepanel\` on Windows, else
`$XDG_CACHE_HOME/tracepanel/`, else `~/.cache/tracepanel/` (macOS too),
else the OS temp directory's `tracepanel/` when there is no home directory.
Behind a proxy, set `HTTPS_PROXY`; with an npm mirror,
`TRACEPANEL_NPM_REGISTRY` (or npm's own `npm_config_registry`).
`TRACEPANEL_AGENT_BIN` names another agent, and without a network the SDK
runs `tracepanel-agent` from the `PATH`.

The SDK and the agent share a minor version: Tracepanel for Go 0.10.x runs
`tracepanel-agent` 0.10.x.

Go 1.26 or later, on Linux, macOS and Windows (`go run`'s binaries are
told apart there too, and the agent is the `.exe` of its win32 package,
kept in `%LOCALAPPDATA%\tracepanel\`).

## What it records

- Each request, named by its route (`GET /books/{id}`, the `ServeMux`
  pattern), with the handler that served it as a `view` span.
- SQL through `database/sql`, whatever runs it (your own queries, sqlx,
  sqlc, GORM or Bun on the `*sql.DB`): each statement with its parameters,
  the rows it returned and the line of your code that ran it, and each
  transaction around its statements.
- `slog` records logged with the request's context
  (`slog.InfoContext(ctx, ...)`), with the line that logged them.
- Calls to HTTP APIs made with the request's context
  (`http.NewRequestWithContext(ctx, ...)`) through `http.DefaultClient` or
  a client with `tracepanel.WrapTransport(base)`, each with a
  `traceparent`, so the app it reaches links its trace to the call.
  `http.Get(url)` carries no context, so it isn't recorded. When the
  panel's Profile asked for the request, its calls carry
  `X-Tracepanel-Profile`, so the apps down the chain profile their part.
- Panics, with your frames and the source around them. The request
  answers 500.
- The caller's trace, when a request carries a W3C `traceparent`.
- What the request carried, for the console's Request tab: its headers,
  query, cookies, the body as far as your handler read it (up to 16 KB;
  JSON and forms with their secrets masked, text as it is, binary left
  out) and the response's headers. Values under a name that looks
  secret (`Authorization`, `password`, `sessionid`, `api_key`) are
  masked before they are recorded, and none of it leaves your machine.
  A name is read word by word (camelCase, `snake_case`, `kebab-case`,
  dots), as every Tracepanel SDK reads it: `authToken`, `AUTH_HEADER`
  and `X-Api-Key` are masked, `Author`, `tokenizer` and `token_type`
  aren't, and `tokens` holding numbers (a model's counts) is shown.
  The SDK never reads a body your handler didn't.
- Where a request's CPU time went, when the panel's Profile asks for it
  (see [Profiles](#profiles)).

Every response carries `Tracepanel-Id` and `Server-Timing`, and an HTML page
gets the badge: a page is held until the handler returns, unless it
flushes it. Any other response (JSON, a file, a stream) goes out as it is
written, and one the handler takes over (a WebSocket) is its own.

The console's App tab shows Go, the router, your module and the modules it
depends on, and the build's settings that hold no secret (never
`-ldflags`).

A statement, a log record or a call joins the request's trace through its
context: pass `r.Context()` down, as in `db.QueryContext(ctx, ...)`. Work
without it isn't recorded.

### Other routers

`ServeMux` names each route. gin and Echo have middleware of their own
(see [Integrations](#integrations)). Another router takes
`tracepanel.Middleware`, and `Config.Route` returns the route, as here for
chi:

```go
tracepanel.Setup(tracepanel.Config{
	Route: func(r *http.Request) string {
		return chi.RouteContext(r.Context()).RoutePattern()
	},
})
router.Use(tracepanel.Middleware)
```

### Profiles

The panel's Profile toggle (or a call carrying `X-Tracepanel-Profile: 1`)
records the next requests with a profile: a flame graph of where their CPU
time went, from Go's own profiler (`runtime/pprof`). The request's
goroutine carries a profiler label, and so do the goroutines it starts, so
a profile holds only that request's samples, however many requests run at
once. A task it starts with `tracepanel.Go` is profiled too.
`TRACEPANEL_PROFILE=1` (or `Config.Profile`) profiles every trace, and
your code can profile the rest of the trace it runs in:

```go
ctx = tracepanel.Profile(ctx)
```

A profiled trace says why in `profile.reason`: `header` (a request's
`X-Tracepanel-Profile`), `cookie` (the panel's toggle), `parent` (a
profiled caller asked: a traced call or message, or a task it started),
`setting` or `code`. When `TRACEPANEL_PROFILE=1` would profile the trace
anyway, the reason is `setting`, whatever asked too. Otherwise it is the
first ask that profiled the trace, and a later one (`tracepanel.Profile` in
a trace its request's header profiled) doesn't change it.

Go's profiler samples 100 times a second of CPU time: a request that takes
less than 10 ms of CPU may have no sample, and time spent waiting (for the
database, a lock) isn't in the profile, which the trace's spans show. An
app that runs a CPU profile of its own (`/debug/pprof/profile`) gets no
Tracepanel profiles meanwhile.

Go's profiler hands its samples over only once it is stopped, and on
macOS stopping it takes up to 100 ms, when nothing is sampled. So it runs
on from one profiled request to the next, and is cut once none has run
for 200 ms (or, under steady load, every 2 s): back-to-back requests are
sampled without a gap, and a profile reaches the console a moment after
its request. A request that runs while a cut stops the profiler says for
how long (`tracepanel.profile_gap_ms`).

Every profile has the garbage collector's runs while the request ran, and
how long they stopped the program. The panel's Profile → Memory
(`X-Tracepanel-Profile: memory`, or `TRACEPANEL_PROFILE_MEMORY=1` for
every profiled trace) adds what the heap did meanwhile: the bytes it
allocated, its highest level above the start and its level at the end.
Go's heap is the whole process's, and nothing the runtime tells of it is
per goroutine (its heap profile's samples carry no labels), so these are
the heap's numbers while the request ran: the request's own when it ran
alone, and marked shared when another one ran meanwhile. Garbage counts
until the collector frees it, and there are no allocation sites.

### Databases

`tracepanel.Driver` works wherever a driver's name goes: `sqlx.Open`, or
GORM's `DriverName` (or use the [GORM plugin](#gorm), not both). For a
driver's connector, `tracepanel.WrapConnector`
wraps it for `sql.OpenDB`:

```go
db := sql.OpenDB(tracepanel.WrapConnector(stdlib.GetConnector(*config)))
```

The console runs `EXPLAIN` of a recorded `SELECT` on the database it came
from. With a second database, name it:

```go
replicaDriver := tracepanel.Driver("pgx", tracepanel.DBAlias("replica"))
replica, err := sql.Open(replicaDriver, replicaDSN)
```

A transaction's statements go under it. For the rest of its work (an HTTP
call, a log record, a span), the transaction's context puts it there too,
so the console sees a call made inside a transaction:

```go
tx, err := db.BeginTx(ctx, nil)
ctx = tracepanel.TxContext(ctx, tx)
```

## Integrations

Each is a module of its own, so the core keeps no dependencies:

| Library | Module | What it records |
| --- | --- | --- |
| gin | `github.com/tracepanel/tracepanel-go/gintrace` | requests, named by route |
| Echo v4 | `github.com/tracepanel/tracepanel-go/echotrace` | requests, named by route |
| Echo v5 | `github.com/tracepanel/tracepanel-go/echo5trace` | requests, named by route |
| pgx v5 | `github.com/tracepanel/tracepanel-go/pgxtrace` | SQL and transactions |
| GORM v2 | `github.com/tracepanel/tracepanel-go/gormtrace` | SQL and transactions |
| go-redis v9 | `github.com/tracepanel/tracepanel-go/redistrace` | Redis commands |
| zap | `github.com/tracepanel/tracepanel-go/zaptrace` | log records |
| zerolog | `github.com/tracepanel/tracepanel-go/zerologtrace` | log records |
| gRPC | `github.com/tracepanel/tracepanel-go/grpctrace` | calls served and made |
| kafka-go | `github.com/tracepanel/tracepanel-go/kafkatrace` | messages sent and handled |

```bash
go get github.com/tracepanel/tracepanel-go/gintrace
```

Fiber runs on fasthttp, not `net/http`, and isn't supported yet.

### gin

```go
router := gin.Default()
router.Use(gintrace.Middleware())
```

Each request is named by its route (`GET /books/:id`), with its handler as
a `view` span. Pass `c.Request.Context()` down (or `c` itself, with the
engine's `ContextWithFallback` on).

### Echo

```go
e := echo.New()
e.Use(echotrace.Middleware())  // echo5trace.Middleware() for Echo v5
```

Each request is named by its route (`GET /books/:id`); on Echo v4 its
handler is a `view` span. An error a handler returns is answered by Echo's
error handler within the trace. Pass `c.Request().Context()` down.

### pgx

The tracer records each statement as `tracepanel.Driver` does, on a
`pgx.Conn` or a `pgxpool.Pool`:

```go
config, err := pgxpool.ParseConfig(dsn)
config.ConnConfig.Tracer = pgxtrace.Tracer()
pool, err := pgxpool.NewWithConfig(ctx, config)
```

A transaction's statements go under it. For the rest of its work,
`pgxtrace.TxContext(ctx, tx)` puts it there too. A batch's queries are
recorded as they are read, and a `CopyFrom` as one statement.
`pgxtrace.Tracer` takes the options `tracepanel.Driver` takes
(`tracepanel.DBAlias` for a second database), and the console runs
`EXPLAIN` on the database a query came from.

### GORM

```go
db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
err = db.Use(gormtrace.Plugin())
```

The plugin records each statement as `tracepanel.Driver` does, with the
line of your code that ran it, and each transaction, GORM's own around a
create, update or delete too. Run the queries with the request's context,
`db.WithContext(ctx)`. For the rest of a transaction's work,
`gormtrace.TxContext(ctx, tx)` puts it under the transaction. The
database's kind comes from the dialector (`postgres`, `mysql`, `sqlite`,
`sqlserver`, `clickhouse`); `gormtrace.Plugin` takes the options
`tracepanel.Driver` takes, and the console runs `EXPLAIN` on the database
a query came from.

The rows a query returned are there for `Find`, `First`, `Pluck`, `Count`
and for `Scan` (`db.Raw(...).Scan(&rows)`), whose count GORM tells its
logger: the plugin wraps the `gorm.Config.Logger` to read it (a
`Session` given a logger of its own after `db.Use` goes without). Not for
`Rows()` and `Row()`: your code reads those rows from the `*sql.Rows` that
`database/sql` hands over, which nothing can watch. With
`tracepanel.Driver` as the dialector's `DriverName` (in place of the
plugin), the driver counts them as it hands each row over.

Use the plugin or `tracepanel.Driver` as the dialector's `DriverName`, not
both. A database opened through `tracepanel.Driver` is recorded already,
and the plugin leaves it to it.

### go-redis

```go
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
redistrace.Instrument(rdb)
```

Each command is a span with its key, the database and the server, so the
agent finds a command repeated for each item (N+1) and duplicates. A
pipeline is one span. Cluster and ring clients work too, without the
server.

### zap and zerolog

zap's loggers carry no context: `zaptrace.Context(ctx)` is a field that
names it, on a record or on a logger, and writes nothing.

```go
logger = logger.WithOptions(zap.WrapCore(zaptrace.WrapCore))
logger.Info("order placed", zaptrace.Context(ctx), zap.Int("order", id))
```

zerolog takes a writer and a hook, both:

```go
logger := zerolog.New(zerologtrace.Writer(os.Stdout)).Hook(zerologtrace.Hook{})
logger.Info().Ctx(ctx).Int("order", id).Msg("order placed")
```

A record keeps its level, its message, its fields (secret-looking ones
masked, as `slog`'s are) and the line that logged it, when the logger adds
the caller.

### gRPC

```go
server := grpc.NewServer(grpctrace.ServerOptions()...)
options := append(grpctrace.DialOptions(), grpc.WithTransportCredentials(creds))
conn, err := grpc.NewClient(target, options...)
```

Each call served is a trace of its own, named `Service/Method`; a server's
fault (`Internal`, `Unavailable`, a panic) fails it. Each call made is a
span of the trace in its context, and carries a `traceparent` in its
metadata, so the server links its trace to it. A call asks the server to
profile its trace with the `x-tracepanel-profile` metadata key (`1`, as
the HTTP header asks), and a call made in a profiled trace asks the same,
so the servers down the chain profile their part. The interceptors are there
one by one too (`UnaryServerInterceptor`, `StreamClientInterceptor`...),
for a chain: a recovery interceptor goes before them.

### Kafka

segmentio/kafka-go's writer, wrapped, records what it sends; a consumer
hands each message to `kafkatrace.Process`:

```go
writer := kafkatrace.WrapWriter(&kafka.Writer{Addr: kafka.TCP(broker), Topic: "orders"})
err := writer.WriteMessages(ctx, kafka.Message{Value: order})

msg, err := reader.FetchMessage(ctx)
err = kafkatrace.Process(ctx, msg, handleOrder)
```

The messages a call sends to a topic are a span (`publish orders`), and
each carries a `traceparent`. Each message handled is a task trace of its
own, named after its handler and linked to the trace that sent it, even
in another app with Tracepanel. A message sent in a profiled trace carries
`tracepanel-profile: 1`, as the other SDKs' messages do, and the trace
that handles it is profiled too.

## Your own code, tasks and commands

```go
// A span of the request's trace, until end().
ctx, end := tracepanel.Span(ctx, "render invoice")
defer end()

// A task: a trace of its own on a goroutine, linked to the request that started it.
tracepanel.Go(ctx, "send welcome email", func(ctx context.Context) error {
	return mailer.Send(ctx, user)
})

// The signed-in user, by id and username, from your authentication.
tracepanel.SetUser(ctx, strconv.FormatInt(user.ID, 10), user.Email)

// A command or a scheduled job: a trace of its own.
err := tracepanel.Command(ctx, "nightly report", report)
```

A program without an HTTP server (a worker, a command line tool) calls
`tracepanel.Setup()` first, and `tracepanel.Flush(time.Second)` before it
exits, so its traces reach the agent.

## Dumps

`tracepanel.Dump` is a debug print: each value with the expression that
gave it, as Go syntax, its type and its length, on stderr. In a request,
the dump is in its trace too, in the call tree where it ran.

```go
tracepanel.Dump(ctx, order, user.Roles)
```

```text
handlers.go:42 checkout
    order: &main.Order{
        ID:    7,
        Items: []main.Item{},
    } (*main.Order)
    user.Roles: []string{"admin"} ([]string) len=1
```

It prints in every build, recorded or not. A value whose expression names a
secret (`password`, `token`, `apiKey`) is masked, and so is a value inside
one under such a name: a struct's field (`Password`, `APIKey`) or a map's
key (`"session_id"`), printed and recorded alike. A long value is cut at
16 KB.

## Settings

`tracepanel.WrapHandler(mux, tracepanel.Config{...})` and
`tracepanel.Setup(...)` take a `Config`; with `Enabled`, call `Setup` before
`tracepanel.Driver`, so the database is recorded too. What it leaves empty
comes from the environment:

| Field | Variable | Default |
| --- | --- | --- |
| `Enabled` | `TRACEPANEL_ENABLED` | on under `go run`, off in a built binary; `1` records in any build, `0` never |
| `Service` | `TRACEPANEL_SERVICE` | the executable's name |
| `StoreDir` | `TRACEPANEL_DIR` | `.tracepanel`, the project's store |
| `ConsoleURL` | `TRACEPANEL_CONSOLE_URL` | where the project's agent serves it |
| `MaxTraces` | `TRACEPANEL_MAX_TRACES` | `2000`, the traces the agent keeps |
| `Profile` | `TRACEPANEL_PROFILE` | off; `1` profiles every trace, not only those the panel asks for |
| `ProfileMemory` | `TRACEPANEL_PROFILE_MEMORY` | off; `1` measures every profiled trace's memory, not only those asked for it |
| | `TRACEPANEL_PROFILE_ENABLED` | on; `0` profiles nothing |
| | `TRACEPANEL_CAPTURE_REQUEST` | on; `0` keeps no headers, query, cookies or body |
| | `TRACEPANEL_AGENT_BIN` | the one the SDK downloads |
| | `TRACEPANEL_AGENT` | `auto`; an agent elsewhere (`tcp:host:port`), never started; `off` starts none |
| | `TRACEPANEL_CACHE_DIR` | `%LOCALAPPDATA%\tracepanel` on Windows, else `$XDG_CACHE_HOME/tracepanel` or `~/.cache/tracepanel`, else the OS temp directory's `tracepanel` when there is no home directory, where the downloaded agent is kept |
| | `TRACEPANEL_NPM_REGISTRY` | npm's registry, where the agent is downloaded from |
| | `TRACEPANEL_N_PLUS_ONE_THRESHOLD` | `3`, repeats that make an N+1 |
| | `TRACEPANEL_N_PLUS_ONE_MIN_MS` | `5`, the time repeated cache calls must cost together |
| | `TRACEPANEL_SLOW_SQL_MS` | `100`, a slow query |
| | `TRACEPANEL_LONG_TRANSACTION_MS` | `500`, a long transaction |
| | `TRACEPANEL_SLOW_HTTP_MS` | `500`, a slow outgoing call |
| | `TRACEPANEL_QUEUE_WAIT_MS` | `1000`, a task that waited too long for a worker |
| | `TRACEPANEL_LARGE_RESULT_ROWS` | `1000`, a query without a LIMIT that read too many rows |

`TRACEPANEL_SLOW_SQL_MS` and `TRACEPANEL_LONG_TRANSACTION_MS` take a limit
per database too, by its kind: `TRACEPANEL_SLOW_SQL_MS=100,snowflake=3000`
holds every database to 100 ms but Snowflake, to 3 s. Without one, the
agent holds an analytical warehouse (Snowflake, BigQuery, Databricks,
Redshift, ClickHouse) to 2 s per query. The kind comes from the driver's
name: `pgx` and `lib/pq` are `postgresql`, gosnowflake's is `snowflake`.
The drivers of BigQuery, Databricks, Redshift, Trino, ClickHouse, DuckDB,
MySQL, MariaDB, SQLite, SQL Server and Oracle are told the same way. For
another database, `tracepanel.DBSystem("cockroachdb")` names it.

Without an agent (none could be had, or it couldn't start), the log says
why once. Responses keep their headers, pages get no badge, and traces
aren't kept.

## MCP

For an AI coding agent (Claude Code, Cursor, Codex), the same traces over
MCP, from the project's directory, with the agent the SDK runs, through
the `tracepanel` command (`go install
github.com/tracepanel/tracepanel-go/cmd/tracepanel@v0.10.0`):

```bash
claude mcp add --scope project tracepanel -- tracepanel mcp
```

Without installing it, `go run
github.com/tracepanel/tracepanel-go/cmd/tracepanel@v0.10.0 mcp` does the
same. The command runs the agent's console in the foreground
(`tracepanel dev --port 8765`, and `tracepanel dev -- ./shop` with the
app beside it, recording), a built app recording (`tracepanel run
./shop`) and the agent with its own arguments (`tracepanel agent
--help`).

## Development

```bash
make lint       # gofmt, go vet, the tests of every module, every module built for Windows
make cross      # every module vetted and built for Windows, amd64 and arm64
make e2e        # an app against the real agent (e2e/, a module of its own)
make integrity  # the checksums of AgentVersion's binaries, once it is on npm
```

`make integrity` (or `go generate`) writes `internal/agentbin/integrity.go`,
the SHA-512 of each platform's agent binary, after `AgentVersion` changes in
`version.go`; a release checks the tag is `Version`, `AgentVersion` of its
minor, and the checksums of `AgentVersion`. The tests that run the agent
take a build of the sibling `tracepanel` checkout, or the one
`TRACEPANEL_AGENT_BIN` names; `make e2e` downloads it otherwise, as an app
does.

Each integration is a module of its own, with a `replace` of the core to
the checkout, which Go ignores in a dependency. A release tags the core
(`v0.10.0`) and then, on the same commit, each integration at the same
version (`gintrace/v0.10.0`): each must require the core of the tag, and
is built without its `replace` against the core the module proxy has.

## License

MIT
