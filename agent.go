package tracepanel

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Traces go to the tracepanel agent, which finds their issues, keeps them and serves the console; its address is in
// <store dir>/agent.addr. The wire: a hello line, then frames of a 4-byte big-endian length and a JSON array of
// traces.

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
	starter   *starter
	storeDir  string
	maxTraces int
	queue     chan *Trace
	conn      net.Conn
	retryAt   time.Time
	warned    bool
}

func newSender(starter *starter, maxTraces int) *sender {
	s := &sender{starter: starter, storeDir: starter.storeDir, maxTraces: maxTraces, queue: make(chan *Trace, queueSize)}
	go s.run()
	return s
}

// submit never blocks the request: a full queue drops the trace.
func (s *sender) submit(trace *Trace) {
	select {
	case s.queue <- trace:
	default:
	}
}

func (s *sender) run() {
	for first := range s.queue {
		batch := []*Trace{first}
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
	}
}

func (s *sender) send(batch []*Trace) {
	if s.conn == nil && time.Now().After(s.retryAt) {
		s.conn = s.connect()
		if s.conn == nil && s.starter.ensure() {
			// None answered: one was started (once per process).
			s.conn = s.connect()
		}
		if s.conn == nil {
			s.retryAt = time.Now().Add(retryAfter)
		}
	}
	if s.conn == nil {
		// Starting one says why it couldn't; an agent named in TRACEPANEL_AGENT that doesn't answer, here.
		if !s.warned && !autoStart() {
			s.warned = true
			log.Printf("tracepanel: no agent at %s: traces are dropped until it answers", agentAddress(s.storeDir))
		}
		return
	}
	data, err := json.Marshal(batch)
	if err != nil {
		log.Printf("tracepanel: %v", err)
		return
	}
	frame := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(frame, uint32(len(data)))
	copy(frame[4:], data)
	_ = s.conn.SetWriteDeadline(time.Now().Add(sendTimeout))
	if _, err := s.conn.Write(frame); err != nil {
		// The agent went away: look for it again in a few seconds.
		_ = s.conn.Close()
		s.conn, s.retryAt, s.warned = nil, time.Now().Add(retryAfter), false
	}
}

// agentAddress is $TRACEPANEL_AGENT (an agent outside the app's container: tcp:host.docker.internal:7701), or the
// address the project's agent wrote: unix:/path, or tcp:host:port on Windows.
func agentAddress(storeDir string) string {
	if addr := os.Getenv("TRACEPANEL_AGENT"); addr != "" && addr != "auto" {
		return addr
	}
	addr, err := os.ReadFile(filepath.Join(storeDir, "agent.addr"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(addr))
}

// connect returns a connection the agent accepted, or nil.
func (s *sender) connect() net.Conn {
	addr := agentAddress(s.storeDir)
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
	hello, _ := json.Marshal(map[string]any{"protocol": protocol, "max_traces": s.maxTraces})
	var answer struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if _, err := conn.Write(append(hello, '\n')); err != nil {
		conn.Close()
		return nil
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil || json.Unmarshal(line, &answer) != nil || !answer.OK {
		if answer.Error != "" {
			log.Printf("tracepanel: the agent refused: %s", answer.Error)
		}
		conn.Close()
		return nil
	}
	_ = conn.SetDeadline(time.Time{})
	return conn
}
