package tracepanel

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Traces go to the Tracepanel agent, which finds their issues, keeps them and serves the console; its address is in
// <store dir>/agent.addr. The wire: a hello line and the agent's answer, then frames of a 4-byte big-endian length
// and JSON both ways: up, a list of traces or an object answering a request; down, the agent's requests (an
// EXPLAIN).

const (
	protocol       = 1
	queueSize      = 1000
	batchSize      = 50
	batchWait      = 50 * time.Millisecond
	connectTimeout = 500 * time.Millisecond
	sendTimeout    = 2 * time.Second
	retryAfter     = 5 * time.Second
)

type sender struct {
	starter *starter
	config  Config
	queue   chan *traceDoc
	// Traces taken from the queue and not sent yet: Flush waits for none.
	pending sync.WaitGroup

	mu      sync.Mutex // the connection, and writes on it
	conn    net.Conn
	retryAt time.Time
	warned  bool
	// Whether the connection's hello said the app answers EXPLAIN: once it has a database, it connects again to say so.
	saidExplain bool
	// What the app does beyond EXPLAIN, which the hello says: request, profile, profile_all.
	capabilities []string
}

func newSender(starter *starter, config Config) *sender {
	s := &sender{starter: starter, config: config, queue: make(chan *traceDoc, queueSize)}
	go s.run()
	return s
}

// submit never blocks the request: a full queue drops the trace.
func (s *sender) submit(trace *traceDoc) {
	s.pending.Add(1)
	select {
	case s.queue <- trace:
	default:
		s.pending.Done()
	}
}

// flush waits until every trace submitted went out (or was given up on), at most `timeout`.
func (s *sender) flush(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func (s *sender) run() {
	for first := range s.queue {
		batch := []*traceDoc{first}
		deadline := time.After(batchWait)
	collect:
		for len(batch) < batchSize {
			select {
			case trace := <-s.queue:
				batch = append(batch, trace)
			case <-deadline:
				break collect
			}
		}
		s.send(batch)
		for range batch {
			s.pending.Done()
		}
	}
}

func (s *sender) send(batch []*traceDoc) {
	conn := s.connection()
	if conn == nil {
		return
	}
	data, err := json.Marshal(batch)
	if err != nil {
		log.Printf("tracepanel: %v", err)
		return
	}
	if err := s.write(conn, data); err != nil {
		// The agent went away: look for it again in a few seconds.
		s.drop(conn)
	}
}

// connection is the agent's connection, made (and the agent started) when there is none.
func (s *sender) connection() net.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil && !s.saidExplain && hasDatabases() {
		// The app has a database now, which it runs EXPLAIN on: the agent hears a new hello that says so.
		_ = s.conn.Close()
		s.conn, s.retryAt = nil, time.Time{}
	}
	if s.conn != nil || time.Now().Before(s.retryAt) {
		return s.conn
	}
	s.conn = s.connect()
	if s.conn == nil && s.starter.ensure() {
		// None answered: one was started (once per process).
		s.conn = s.connect()
	}
	if s.conn == nil {
		s.retryAt = time.Now().Add(retryAfter)
		// Starting one says why it couldn't; an agent named in TRACEPANEL_AGENT that doesn't answer, here.
		if !s.warned && !autoStart() {
			s.warned = true
			log.Printf("tracepanel: no agent at %s: traces are dropped until it answers", agentAddress(s.config.StoreDir))
		}
	}
	return s.conn
}

func (s *sender) drop(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = conn.Close()
	if s.conn == conn {
		s.conn, s.retryAt, s.warned = nil, time.Now().Add(retryAfter), false
	}
}

// write sends one frame: traces and answers share the connection, a whole frame at a time.
func (s *sender) write(conn net.Conn, data []byte) error {
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	copy(frame[4:], data)
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(sendTimeout))
	_, err := conn.Write(frame)
	return err
}

// agentAddress is $TRACEPANEL_AGENT (an agent outside the app's container: tcp:host.docker.internal:7701), or the
// address the project's agent wrote: unix:/path, or tcp:host:port on Windows.
func agentAddress(storeDir string) string {
	if addr := os.Getenv("TRACEPANEL_AGENT"); addr != "" && addr != "auto" && addr != "off" {
		return addr
	}
	addr, err := os.ReadFile(filepath.Join(storeDir, "agent.addr"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(addr))
}

// connect returns a connection the agent accepted, or nil; the agent's requests are read from it from then on.
func (s *sender) connect() net.Conn {
	addr := agentAddress(s.config.StoreDir)
	var network, target string
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, target = "unix", path
	} else if hostPort, ok := strings.CutPrefix(addr, "tcp:"); ok {
		network, target = "tcp", hostPort
	} else {
		return nil
	}
	conn, err := net.DialTimeout(network, target, connectTimeout)
	if err != nil {
		return nil
	}
	_ = conn.SetDeadline(time.Now().Add(connectTimeout))
	// The agent finds the issues with this project's limits, shows the app in the console's App tab, and asks this
	// app for EXPLAIN once it has a database to run it on.
	databases := hasDatabases()
	capabilities := []string{}
	if databases {
		capabilities = append(capabilities, "explain")
	}
	capabilities = append(capabilities, s.capabilities...)
	hello, _ := json.Marshal(map[string]any{
		"protocol": protocol, "max_traces": s.config.MaxTraces, "thresholds": thresholds(),
		"service": s.config.Service, "capabilities": capabilities, "app": aboutApp(s.config),
	})
	var answer struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if _, err := conn.Write(append(hello, '\n')); err != nil {
		conn.Close()
		return nil
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil || json.Unmarshal(line, &answer) != nil || !answer.OK {
		if answer.Error != "" {
			log.Printf("tracepanel: the agent refused: %s", answer.Error)
		}
		conn.Close()
		return nil
	}
	_ = conn.SetDeadline(time.Time{})
	s.saidExplain = databases
	// What came with the answer is the start of the first request: the same reader goes on.
	go s.serve(conn, reader)
	return conn
}

// agentRequest is what the agent asks of the app, under the id its answer carries back.
type agentRequest struct {
	ID      json.Number     `json:"id"`
	Explain *explainRequest `json:"explain"`
}

// serve answers the agent's requests until the connection closes.
func (s *sender) serve(conn net.Conn, reader *bufio.Reader) {
	header := make([]byte, 4)
	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(header))
		if _, err := io.ReadFull(reader, body); err != nil {
			return
		}
		var request agentRequest
		decoder := json.NewDecoder(bytes.NewReader(body))
		// A parameter keeps its number as it was: 7 an integer, not 7.0.
		decoder.UseNumber()
		if decoder.Decode(&request) != nil || request.Explain == nil {
			// A request this SDK can't read: the agent times it out.
			continue
		}
		go s.answer(conn, request)
	}
}

func (s *sender) answer(conn net.Conn, request agentRequest) {
	reply := map[string]any{"answer": request.ID}
	if answer, err := explain(*request.Explain); err != nil {
		reply["error"] = err.Error()
	} else {
		reply["columns"], reply["rows"] = answer.Columns, answer.Rows
	}
	data, err := json.Marshal(reply)
	if err != nil {
		data, _ = json.Marshal(map[string]any{"answer": request.ID, "error": err.Error()})
	}
	_ = s.write(conn, data)
}
