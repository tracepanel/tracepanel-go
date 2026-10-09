# tracepanel-go

**See what your Go app is really doing.** tracepanel for Go: every request with its SQL, logs and panics, the
N+1 queries and slow SQL in it, in a badge on your pages and a console.

It builds on OpenTelemetry: your app traces itself with OTel (its HTTP server, `database/sql` through
[otelsql](https://github.com/XSAM/otelsql)), and tracepanel turns each request's spans into a trace for the
tracepanel agent, which finds the issues, keeps the traces in `.tracepanel/` and serves the console.

```bash
go get github.com/tracepanel/tracepanel-go
```

```go
import (
	"github.com/XSAM/otelsql"
	tracepanel "github.com/tracepanel/tracepanel-go"
)

db, err := otelsql.Open("sqlite", path) // in place of sql.Open
// ...
http.ListenAndServe(addr, tracepanel.Wrap(mux))
```

Then run the agent in the project's directory, and open http://127.0.0.1:8765/:

```bash
tracepanel dev
```

## What it records

- Each request: method, route (the `ServeMux` pattern), status, time, and the handler as a `view` span.
- SQL through otelsql: the statement, its fingerprint and the line of your code that ran it.
- `slog` records, attached to the request that logged them.
- Panics: the value, the stack with the source around your frames; the request answers 500.
- The badge's headers on every response (`Tracepanel-Id`, `Server-Timing`) and `widget.js` in HTML pages.

The agent finds the issues: N+1 queries, slow SQL, errors. otelsql doesn't record a statement's parameters, so an
N+1 is found by its shape (a lookup by `id` looped from one line) and duplicate queries aren't reported.

## Settings

`tracepanel.Wrap(mux, tracepanel.Config{...})`; what a `Config` leaves empty comes from the environment:

| Field | Variable | Default |
| --- | --- | --- |
| `Service` | | the executable's name |
| `StoreDir` | `TRACEPANEL_DIR` | `.tracepanel` |
| `ConsoleURL` | `TRACEPANEL_CONSOLE_URL` | `http://127.0.0.1:8765/` |
| | `TRACEPANEL_AGENT` | the address in `<StoreDir>/agent.addr`; `tcp:host.docker.internal:PORT` for an app in a container |

The app's own `TracerProvider` keeps its exporters: tracepanel is one more span processor on it.

## Development

```bash
make lint    # gofmt, go vet, go test
```

`tracepanel-examples/go-api` is a books API using it (Swagger UI with the panel under it).
