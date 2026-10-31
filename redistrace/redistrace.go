// Package redistrace records the commands of go-redis v9 as `redis` spans, as every Tracepanel SDK records Redis's:
// the command, its key, the database and the server, so the agent finds repeated commands (N+1) and duplicates.
//
//	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
//	redistrace.Instrument(rdb)
//
// A command is recorded when it runs with the context of a traced request: rdb.Get(ctx, key).
package redistrace

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// Instrument records the commands of a client: a *redis.Client, a *redis.ClusterClient, a *redis.Ring, or one
// redis.NewUniversalClient made. A client's server and database go with its commands; a cluster's or a ring's, whose
// commands go to several servers, don't.
func Instrument(client redis.UniversalClient) {
	tracer := &tracer{}
	if single, ok := client.(*redis.Client); ok {
		tracer.where = where(single.Options())
	}
	client.AddHook(tracer)
}

type tracer struct {
	// server.address, server.port and db.namespace, and the alias the agent groups commands by.
	where map[string]any
}

var _ redis.Hook = (*tracer)(nil)

// where is a client's server and database, as the other SDKs name them.
func where(options *redis.Options) map[string]any {
	out := map[string]any{"redis.alias": "redis"}
	if options.Network == "unix" {
		out["server.address"] = options.Addr
	} else if host, port, err := net.SplitHostPort(options.Addr); err == nil {
		out["server.address"] = host
		if number, err := strconv.Atoi(port); err == nil {
			out["server.port"] = number
		}
	}
	if options.DB != 0 {
		out["db.namespace"] = strconv.Itoa(options.DB)
		out["redis.alias"] = "redis/" + strconv.Itoa(options.DB)
	}
	return out
}

func (t *tracer) DialHook(next redis.DialHook) redis.DialHook { return next }

func (t *tracer) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if !hook.Enabled() || connecting(cmd) {
			return next(ctx, cmd)
		}
		command := strings.ToUpper(cmd.Name())
		key := keyOf(cmd.Args())
		name := command
		if key != nil {
			name = fmt.Sprintf("%s %v", command, key)
		}
		span := hook.Begin(ctx, "redis", name, t.attrs(command, key))
		if span == nil {
			return next(ctx, cmd)
		}
		err := next(ctx, cmd)
		end(span, err)
		return err
	}
}

// ProcessPipelineHook records a pipeline (or a MULTI/EXEC transaction) as one span: its commands go to the server
// together. Its key is how many commands it sent.
func (t *tracer) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if !hook.Enabled() || all(cmds, connecting) {
			return next(ctx, cmds)
		}
		span := hook.Begin(ctx, "redis", "PIPELINE", t.attrs("PIPELINE", len(cmds)))
		if span == nil {
			return next(ctx, cmds)
		}
		err := next(ctx, cmds)
		end(span, err)
		return err
	}
}

// connecting tells the commands go-redis sends on a new connection before the app's (HELLO, AUTH, SELECT, CLIENT
// SETNAME): the client's own, not the app's.
func connecting(cmd redis.Cmder) bool {
	switch strings.ToLower(cmd.Name()) {
	case "hello", "auth", "select", "readonly", "client":
		return true
	}
	return false
}

func all(cmds []redis.Cmder, test func(redis.Cmder) bool) bool {
	for _, cmd := range cmds {
		if !test(cmd) {
			return false
		}
	}
	return len(cmds) > 0
}

func (t *tracer) attrs(command string, key any) map[string]any {
	attrs := map[string]any{"db.system": "redis", "db.operation": command, "redis.key": key}
	for name, value := range t.where {
		attrs[name] = value
	}
	if attrs["redis.alias"] == nil {
		attrs["redis.alias"] = "redis"
	}
	if origin := hook.Origin(); origin != "" {
		attrs["code.origin"] = origin
	} else {
		attrs["code.origin"] = nil
	}
	return attrs
}

// end ends a command's span: a missing key (redis.Nil) is an answer, not a failure.
func end(span hook.Span, err error) {
	failed := err != nil && err != redis.Nil
	if failed {
		span.Set("error.type", fmt.Sprintf("%T", err))
	}
	span.End(failed)
}

// keyOf is a command's key, its first argument after the name: a string or an integer, nil for none.
func keyOf(args []any) any {
	if len(args) < 2 {
		return nil
	}
	switch key := args[1].(type) {
	case string:
		return key
	case []byte:
		return string(key)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return key
	case nil:
		return nil
	default:
		return fmt.Sprint(key)
	}
}
