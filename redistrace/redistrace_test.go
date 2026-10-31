package redistrace

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

// fakeRedis answers RESP2 commands over a pipe: SET keeps a value, GET reads it (nil when missing), FAIL fails.
type fakeRedis struct {
	mu     sync.Mutex
	values map[string]string
}

func (f *fakeRedis) serve(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		args, err := readCommand(reader)
		if err != nil {
			return
		}
		var reply string
		f.mu.Lock()
		switch strings.ToUpper(args[0]) {
		case "PING":
			reply = "+PONG\r\n"
		case "SET":
			f.values[args[1]] = args[2]
			reply = "+OK\r\n"
		case "GET":
			if value, ok := f.values[args[1]]; ok {
				reply = fmt.Sprintf("$%d\r\n%s\r\n", len(value), value)
			} else {
				reply = "$-1\r\n"
			}
		case "INCR":
			reply = ":1\r\n"
		case "FAIL", "HELLO":
			reply = "-ERR no such command\r\n"
		default:
			reply = "+OK\r\n"
		}
		f.mu.Unlock()
		if _, err := conn.Write([]byte(reply)); err != nil {
			return
		}
	}
}

func readCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	args := make([]string, 0, count)
	for range count {
		if _, err := reader.ReadString('\n'); err != nil {
			return nil, err
		}
		arg, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		args = append(args, strings.TrimSuffix(arg, "\r\n"))
	}
	return args, nil
}

func client(t *testing.T, db int) *redis.Client {
	fake := &fakeRedis{values: map[string]string{}}
	rdb := redis.NewClient(&redis.Options{
		Addr: "cache.internal:6380", DB: db, Protocol: 2, DisableIdentity: true,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			conn, server := net.Pipe()
			go fake.serve(server)
			return conn, nil
		},
	})
	Instrument(rdb)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func command(t *testing.T, recorder *tracetest.Recorder, fn func(ctx context.Context) error) tracetest.Doc {
	t.Helper()
	if err := tracepanel.Command(context.Background(), "job", fn); err != nil {
		t.Fatal(err)
	}
	return recorder.Wait(t, 1)[0]
}

func TestCommandsAreRedisSpans(t *testing.T) {
	recorder := tracetest.Record(t)
	rdb := client(t, 0)

	trace := command(t, recorder, func(ctx context.Context) error {
		if err := rdb.Set(ctx, "book:7", "Dune", 0).Err(); err != nil {
			return err
		}
		if err := rdb.Get(ctx, "book:8").Err(); err != redis.Nil {
			return fmt.Errorf("get: %v", err)
		}
		return nil
	})

	spans := tracetest.Spans(trace, "redis")
	if len(spans) != 2 {
		t.Fatalf("redis spans: %v", trace["spans"])
	}
	attrs := tracetest.Attrs(spans[0])
	origin, _ := attrs["code.origin"].(string)
	if !strings.HasPrefix(origin, "redistrace_test.go:") {
		t.Fatalf("code.origin: %v", attrs["code.origin"])
	}
	delete(attrs, "code.origin")
	want := tracetest.Doc{
		"db.system": "redis", "db.operation": "SET", "redis.key": "book:7", "redis.alias": "redis",
		"server.address": "cache.internal", "server.port": 6380,
	}
	if spans[0]["name"] != "SET book:7" || !tracetest.Equal(attrs, want) {
		t.Fatalf("SET: %v %v", spans[0]["name"], attrs)
	}
	// A missing key is an answer.
	if spans[1]["name"] != "GET book:8" || spans[1]["status"] != "ok" {
		t.Fatalf("GET: %v", spans[1])
	}
}

func TestADatabaseOtherThanZeroIsItsOwnAlias(t *testing.T) {
	recorder := tracetest.Record(t)
	rdb := client(t, 2)

	trace := command(t, recorder, func(ctx context.Context) error {
		return rdb.Incr(ctx, "views").Err()
	})

	spans := tracetest.Spans(trace, "redis")
	attrs := tracetest.Attrs(spans[len(spans)-1])
	if attrs["redis.alias"] != "redis/2" || attrs["db.namespace"] != "2" || attrs["db.operation"] != "INCR" {
		t.Fatalf("attrs: %v", attrs)
	}
}

func TestAPipelineIsOneSpan(t *testing.T) {
	recorder := tracetest.Record(t)
	rdb := client(t, 0)

	trace := command(t, recorder, func(ctx context.Context) error {
		_, err := rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Set(ctx, "a", "1", 0)
			pipe.Set(ctx, "b", "2", 0)
			pipe.Get(ctx, "a")
			return nil
		})
		return err
	})

	spans := tracetest.Spans(trace, "redis")
	if len(spans) != 1 || spans[0]["name"] != "PIPELINE" || tracetest.Attrs(spans[0])["redis.key"] != float64(3) {
		t.Fatalf("spans: %v", spans)
	}
}

func TestAFailedCommand(t *testing.T) {
	recorder := tracetest.Record(t)
	rdb := client(t, 0)

	trace := command(t, recorder, func(ctx context.Context) error {
		_ = rdb.Do(ctx, "FAIL", "x").Err()
		return nil
	})

	span := tracetest.Spans(trace, "redis")[0]
	if span["name"] != "FAIL x" || span["status"] != "error" || tracetest.Attrs(span)["error.type"] != "proto.RedisError" {
		t.Fatalf("span: %v", span)
	}
}

func TestOutsideATraceNothingIsRecorded(t *testing.T) {
	recorder := tracetest.Record(t)
	rdb := client(t, 0)

	if err := rdb.Set(context.Background(), "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	trace := command(t, recorder, func(context.Context) error { return nil })

	if spans := tracetest.Spans(trace, "redis"); len(spans) != 0 {
		t.Fatalf("recorded: %v", spans)
	}
}
