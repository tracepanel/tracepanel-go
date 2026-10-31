# Tracepanel for Go

Tracepanel shows what each request of your app did: its SQL with the line of
your code that ran each statement, its logs, its outgoing calls and its
panics. It finds what to fix: N+1 queries, duplicates, slow SQL, long
transactions. A badge on your pages opens the panel, and the console lists
every request.

This SDK records a Go app's requests and sends them to Tracepanel's agent,
which keeps them, finds the issues and serves the console. It has no
dependencies: the integrations with gin, Echo, pgx, go-redis, zap, zerolog,
gRPC and Kafka are modules of their own, and you get only those you use.

## Setup

```bash
go get github.com/tracepanel/tracepanel-go
```

Give `sql.Open` the driver's name through `tracepanel.WrapDriver`, and wrap
the app's handler:

```go
import tracepanel "github.com/tracepanel/tracepanel-go"

db, err := sql.Open(tracepanel.WrapDriver("pgx"), dsn)
// ...
http.ListenAndServe(addr, tracepanel.WrapHandler(mux))
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
  client with `tracepanel.WrapTransport(base)`, each with a `traceparent`, so
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

### Databases

`tracepanel.WrapDriver` works wherever a driver's name goes: `sqlx.Open`, or
GORM's `DriverName`. For a driver's connector, `tracepanel.WrapConnector`
wraps it for `sql.OpenDB`:

```go
db := sql.OpenDB(tracepanel.WrapConnector(stdlib.GetConnector(*config)))
```

The console runs `EXPLAIN` of a recorded `SELECT` on the database it came
from. With a second database, name it:

```go
replicaDriver := tracepanel.WrapDriver("pgx", tracepanel.DBAlias("replica"))
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

The tracer records each statement as `WrapDriver` does, on a `pgx.Conn` or
a `pgxpool.Pool`:

```go
config, err := pgxpool.ParseConfig(dsn)
config.ConnConfig.Tracer = pgxtrace.New()
pool, err := pgxpool.NewWithConfig(ctx, config)
```

A transaction's statements go under it. For the rest of its work,
`pgxtrace.TxContext(ctx, tx)` puts it there too. A batch's queries are
recorded as they are read, and a `CopyFrom` as one statement.
`pgxtrace.New` takes the options `WrapDriver` takes (`tracepanel.DBAlias`
for a second database), and the console runs `EXPLAIN` on the database a
query came from.

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
metadata, so the server links its trace to it. The interceptors are there
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
in another app with Tracepanel.

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

## Settings

`tracepanel.WrapHandler(mux, tracepanel.Config{...})` and
`tracepanel.Setup(...)` take a `Config`. What it leaves empty comes from the environment:

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
make lint    # gofmt, go vet, the tests of every module
```

Each integration is a module of its own, with a `replace` of the core to
the checkout. A release tags the core (`v0.10.0`) and each integration at
the same version (`gintrace/v0.10.0`).

`go generate` writes `integrity.go`, the checksums of the agent's downloads,
after `AgentVersion` changes in `version.go`. The test that starts the
agent runs with a build of the sibling `tracepanel` checkout, or the one
`TRACEPANEL_AGENT_BIN` names.

## License

MIT
