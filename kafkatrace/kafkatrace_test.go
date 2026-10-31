package kafkatrace

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/segmentio/kafka-go"

	tracepanel "github.com/tracepanel/tracepanel-go"
	"github.com/tracepanel/tracepanel-go/internal/tracetest"
)

// broker keeps the messages a writer sent, in place of Kafka.
type broker struct {
	mu   sync.Mutex
	sent []kafka.Message
	fail error
}

func (b *broker) write(_ context.Context, msgs ...kafka.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.sent = append(b.sent, msgs...)
	return nil
}

func writer(b *broker, topic string) *Writer {
	return &Writer{Writer: &kafka.Writer{Topic: topic}, write: b.write}
}

func handleOrder(ctx context.Context, _ kafka.Message) error {
	_, end := tracepanel.Span(ctx, "charge card")
	end()
	return nil
}

func TestAMessageSentIsLinkedToTheTraceThatHandlesIt(t *testing.T) {
	recorder := tracetest.Record(t)
	b := &broker{}
	w := writer(b, "orders")

	if err := tracepanel.Command(context.Background(), "checkout", func(ctx context.Context) error {
		return w.WriteMessages(ctx, kafka.Message{Value: []byte("1")}, kafka.Message{Value: []byte("2")})
	}); err != nil {
		t.Fatal(err)
	}
	sender := recorder.Wait(t, 1)[0]
	msg := b.sent[0]
	msg.Topic, msg.Partition, msg.Offset = "orders", 3, 41
	if err := Process(context.Background(), msg, handleOrder); err != nil {
		t.Fatal(err)
	}

	publishes := tracetest.Spans(sender, "kafka")
	if len(publishes) != 1 || publishes[0]["name"] != "publish orders" {
		t.Fatalf("publishes: %v", sender["spans"])
	}
	publish := tracetest.Attrs(publishes[0])
	if publish["messaging.system"] != "kafka" || publish["messaging.operation"] != "publish" ||
		publish["messaging.destination.name"] != "orders" || publish["messaging.batch.message_count"] != float64(2) ||
		!strings.HasPrefix(publish["code.origin"].(string), "kafkatrace_test.go:") {
		t.Fatalf("publish: %v", publish)
	}
	if got := header(b.sent[1].Headers, sentByHeader); got != sender["id"].(string)+":"+publishes[0]["id"].(string) {
		t.Fatalf("sent by: %q", got)
	}
	if got := header(b.sent[1].Headers, traceparentHeader); !strings.Contains(got, "-"+publish["w3c.span_id"].(string)+"-") {
		t.Fatalf("traceparent: %q", got)
	}

	task := recorder.Wait(t, 2)[1]
	attrs := tracetest.Attrs(task)
	if task["kind"] != "task" || task["name"] != "handleOrder" || attrs["service.role"] != "worker" {
		t.Fatalf("task: %v %v %v", task["kind"], task["name"], attrs)
	}
	for key, want := range map[string]any{
		"task.system": "kafka", "task.name": "handleOrder", "task.queue": "orders", "task.id": "orders-3@41",
		"task.trigger": "message", "messaging.system": "kafka", "messaging.operation": "process",
		"messaging.destination.name": "orders", "messaging.kafka.partition": float64(3), "messaging.kafka.offset": float64(41),
		"w3c.trace_id": tracetest.Attrs(sender)["w3c.trace_id"],
	} {
		if attrs[key] != want {
			t.Errorf("%s = %v, want %v", key, attrs[key], want)
		}
	}
	if _, ok := attrs["w3c.parent_span_id"]; ok {
		t.Errorf("linked twice: %v", attrs)
	}
	links := tracetest.List(task, "links")
	if len(links) != 1 || links[0]["kind"] != "sent_by" || links[0]["trace_id"] != sender["id"] || links[0]["span_id"] != publishes[0]["id"] {
		t.Fatalf("links: %v", links)
	}
	if work := tracetest.Spans(task, "function"); len(work) != 1 {
		t.Fatalf("work: %v", task["spans"])
	}
}

func TestAFailedWrite(t *testing.T) {
	recorder := tracetest.Record(t)
	w := writer(&broker{fail: errors.New("leader not available")}, "")

	_ = tracepanel.Command(context.Background(), "checkout", func(ctx context.Context) error {
		_ = w.WriteMessages(ctx, kafka.Message{Topic: "orders"}, kafka.Message{Topic: "emails"})
		return nil
	})

	spans := tracetest.Spans(recorder.Wait(t, 1)[0], "kafka")
	if len(spans) != 2 || spans[0]["name"] != "publish orders" || spans[1]["name"] != "publish emails" ||
		spans[0]["status"] != "error" || tracetest.Attrs(spans[0])["error.type"] != "*errors.errorString" {
		t.Fatalf("spans: %v", spans)
	}
}

func TestAMessageKeepsItsOwnTraceparent(t *testing.T) {
	tracetest.Record(t)
	b := &broker{}
	w := writer(b, "orders")
	own := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	original := []kafka.Header{{Key: traceparentHeader, Value: []byte(own)}}

	_ = tracepanel.Command(context.Background(), "relay", func(ctx context.Context) error {
		return w.WriteMessages(ctx, kafka.Message{Headers: original})
	})

	if len(b.sent[0].Headers) != 1 || header(b.sent[0].Headers, traceparentHeader) != own {
		t.Fatalf("headers: %v", b.sent[0].Headers)
	}
}

func TestAFailedHandlerFailsItsTrace(t *testing.T) {
	recorder := tracetest.Record(t)

	err := Process(context.Background(), kafka.Message{Topic: "orders"}, func(context.Context, kafka.Message) error {
		return errors.New("card declined")
	})

	task := recorder.Wait(t, 1)[0]
	errors := tracetest.List(task, "errors")
	if err == nil || task["status"] != "error" || task["name"] != "orders" || len(errors) != 1 ||
		tracetest.Attrs(task)["task.trigger"] != "other" {
		t.Fatalf("task: %v %v %v", task["status"], task["name"], errors)
	}
}

func TestOutsideATraceMessagesAreSentAsTheyAre(t *testing.T) {
	tracetest.Record(t)
	b := &broker{}

	if err := writer(b, "orders").WriteMessages(context.Background(), kafka.Message{Value: []byte("1")}); err != nil {
		t.Fatal(err)
	}

	if len(b.sent) != 1 || len(b.sent[0].Headers) != 0 {
		t.Fatalf("sent: %v", b.sent)
	}
}
