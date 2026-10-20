package main

// Log ring buffer and server-sent-event fan out to the browser.

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type Event struct {
	Type string `json:"type"` // log | rec | done | runstart | runend
	Data any    `json:"data"`
}

type hub struct {
	mu   sync.Mutex
	logs []string
	subs map[chan []byte]struct{}
}

var events = &hub{subs: map[chan []byte]struct{}{}}

var logStdout = true

func logf(f string, a ...any) {
	msg := time.Now().Format("15:04:05.000 ") + fmt.Sprintf(f, a...)
	if logStdout {
		fmt.Fprintln(os.Stdout, msg)
	}
	events.mu.Lock()
	events.logs = append(events.logs, msg)
	if len(events.logs) > 1000 {
		events.logs = events.logs[len(events.logs)-800:]
	}
	events.mu.Unlock()
	events.publish(Event{"log", msg})
}

func (h *hub) publish(e Event) {
	b, _ := json.Marshal(e)
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.subs {
		select {
		case c <- b:
		default: // slow client: drop
		}
	}
}

func (h *hub) subscribe() (chan []byte, []string) {
	c := make(chan []byte, 2048)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[c] = struct{}{}
	return c, append([]string(nil), h.logs...)
}

func (h *hub) unsubscribe(c chan []byte) {
	h.mu.Lock()
	delete(h.subs, c)
	h.mu.Unlock()
}
