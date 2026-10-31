// Package kafkatrace traces Kafka through segmentio/kafka-go with Tracepanel: the messages a Writer sends are a
// `kafka` span of the trace that sent them (publish orders), each carrying a traceparent in its headers; each
// message a consumer handles with Process is a task trace of its own, linked to the publish that sent it.
//
//	writer := kafkatrace.WrapWriter(&kafka.Writer{Addr: kafka.TCP("localhost:9092"), Topic: "orders"})
//	err := writer.WriteMessages(ctx, kafka.Message{Value: order})
//
//	for {
//		msg, err := reader.FetchMessage(ctx)
//		// ...
//		err = kafkatrace.Process(ctx, msg, handleOrder)
//		// ...
//		err = reader.CommitMessages(ctx, msg)
//	}
package kafkatrace

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/segmentio/kafka-go"

	"github.com/tracepanel/tracepanel-go/internal/hook"
)

// The headers a message carries for the app that receives it: the W3C traceparent, and the trace and span that
// sent it, which every Tracepanel SDK links the received message's trace to (sent_by).
const (
	traceparentHeader = "traceparent"
	sentByHeader      = "tracepanel-sent-by"
	taskIDHeader      = "tracepanel-task-id"
)

// Writer is a kafka.Writer whose messages are recorded: WriteMessages is the writer's, with its spans and headers.
type Writer struct {
	*kafka.Writer
	write func(context.Context, ...kafka.Message) error
}

// WrapWriter records the messages `w` sends.
func WrapWriter(w *kafka.Writer) *Writer {
	return &Writer{Writer: w, write: w.WriteMessages}
}

// WriteMessages sends the messages as the writer does. In a trace, the messages of each topic are a `kafka` span
// (publish orders) until the writer took them (an Async writer takes them at once), and carry its traceparent. A
// message that carries a traceparent already keeps it.
func (w *Writer) WriteMessages(ctx context.Context, msgs ...kafka.Message) error {
	if !hook.Enabled() || !hook.Traced(ctx) || len(msgs) == 0 {
		return w.write(ctx, msgs...)
	}
	origin := hook.Origin()
	spans := map[string]hook.Span{}
	var order []hook.Span
	counts := map[string]int{}
	out := make([]kafka.Message, len(msgs))
	for i, msg := range msgs {
		topic := msg.Topic
		if w.Writer != nil && w.Writer.Topic != "" {
			topic = w.Writer.Topic
		}
		span, ok := spans[topic]
		if !ok {
			span = hook.Begin(ctx, "kafka", "publish "+topic, map[string]any{
				"messaging.system": "kafka", "messaging.operation": "publish", "messaging.destination.name": topic,
				"code.origin": nilIfEmpty(origin),
			})
			if span == nil {
				return w.write(ctx, msgs...)
			}
			span.Set("w3c.span_id", span.W3CID())
			spans[topic] = span
			order = append(order, span)
		}
		counts[topic]++
		if header(msg.Headers, traceparentHeader) == "" {
			msg.Headers = append(append(make([]kafka.Header, 0, len(msg.Headers)+2), msg.Headers...),
				kafka.Header{Key: traceparentHeader, Value: []byte(span.Traceparent())},
				kafka.Header{Key: sentByHeader, Value: []byte(span.SentBy())})
		}
		out[i] = msg
	}
	for topic, count := range counts {
		if count > 1 {
			spans[topic].Set("messaging.batch.message_count", count)
		}
	}
	err := w.write(ctx, out...)
	for _, span := range order {
		if err != nil {
			span.Set("error.type", fmt.Sprintf("%T", err))
		}
		span.End(err != nil)
	}
	return err
}

// Process runs `handler` for a message received, as a task trace of its own: named after the handler's function
// (the topic for a function literal), linked to the publish that sent it by its headers. An error it returns, or a
// panic, fails the trace (the panic goes on). Outside Tracepanel, it calls the handler.
func Process(ctx context.Context, msg kafka.Message, handler func(context.Context, kafka.Message) error) (err error) {
	trace := receive(msg, handlerName(handler))
	if trace == nil {
		return handler(ctx, msg)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			trace.Panic(recovered)
			trace.Fail()
			trace.Finish()
			panic(recovered)
		}
		if err != nil {
			trace.Error(err)
			trace.Fail()
		}
		trace.Finish()
	}()
	return handler(trace.Context(ctx), msg)
}

// receive starts a received message's trace, as the other SDKs' (Java's Kafka listeners) start theirs.
func receive(msg kafka.Message, name string) hook.Trace {
	if !hook.Enabled() {
		return nil
	}
	if name == "" {
		name = msg.Topic
	}
	id := header(msg.Headers, taskIDHeader)
	if id == "" {
		id = msg.Topic + "-" + strconv.Itoa(msg.Partition) + "@" + strconv.FormatInt(msg.Offset, 10)
	}
	attrs := map[string]any{
		"task.system": "kafka", "task.name": name, "task.id": id, "task.queue": msg.Topic, "task.retries": 0,
		"messaging.system": "kafka", "messaging.operation": "process", "messaging.destination.name": msg.Topic,
		"messaging.kafka.partition": msg.Partition, "messaging.kafka.offset": msg.Offset,
	}
	caller := hook.Incoming(header(msg.Headers, traceparentHeader))
	sender := strings.Split(header(msg.Headers, sentByHeader), ":")
	linked := len(sender) == 2 && sender[0] != "" && sender[1] != ""
	// Sent by a trace the agent has: linked to it as a delivery. The caller's span would link it a second time.
	if traceID, ok := caller["w3c.trace_id"]; ok {
		attrs["w3c.trace_id"] = traceID
	}
	if !linked {
		for key, value := range caller {
			attrs[key] = value
		}
	}
	attrs["task.trigger"] = "other"
	if linked || caller["w3c.parent_span_id"] != nil {
		attrs["task.trigger"] = "message"
	}
	trace := hook.Start("task", "task.run", name, attrs)
	if trace != nil && linked {
		trace.Link("sent_by", sender[0], sender[1])
	}
	return trace
}

func header(headers []kafka.Header, key string) string {
	for i := len(headers) - 1; i >= 0; i-- {
		if headers[i].Key == key {
			return string(headers[i].Value)
		}
	}
	return ""
}

// handlerName is a handler's short name, "handleOrder" or "(*Orders).handle"; "" for a function literal.
func handlerName(handler any) string {
	name := strings.TrimSuffix(runtime.FuncForPC(reflect.ValueOf(handler).Pointer()).Name(), "-fm")
	if strings.Contains(name[strings.LastIndex(name, "/")+1:], ".func") {
		return ""
	}
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		name = name[slash+1:]
	}
	if _, function, found := strings.Cut(name, "."); found {
		return function
	}
	return name
}

func nilIfEmpty(text string) any {
	if text == "" {
		return nil
	}
	return text
}
