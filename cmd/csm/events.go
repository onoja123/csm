package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	activityFile = "activity.jsonl"
	activityKeep = 500
)

// Event is one message on the service's event stream; Data is already JSON so every subscriber gets the same bytes.
type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
	At   time.Time       `json:"at"`
}

type eventBus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func newEventBus() *eventBus { return &eventBus{subs: map[chan Event]struct{}{}} }

func (b *eventBus) subscribe() (chan Event, func()) {
	ch := make(chan Event, 512)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

func (b *eventBus) subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.subs)
}

func (b *eventBus) publish(kind string, data any) {
	raw, _ := json.Marshal(data)
	ev := Event{Type: kind, Data: raw, At: time.Now()}
	b.mu.Lock()

	defer b.mu.Unlock()

	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Activity is one human-readable thing that happened; it never carries agent output or secrets.
type Activity struct {
	ID       string            `json:"id"`
	At       time.Time         `json:"at"`
	Kind     string            `json:"kind"`
	Provider string            `json:"provider,omitempty"`
	Account  string            `json:"account,omitempty"`
	Title    string            `json:"title"`
	Detail   string            `json:"detail,omitempty"`
	RunID    string            `json:"run_id,omitempty"`
	Refs     map[string]string `json:"refs,omitempty"`
}

type activityLog struct {
	mu      sync.Mutex
	path    string
	entries []Activity
	seq     int
	bus     *eventBus
}

func newActivityLog(home string, bus *eventBus) *activityLog {
	l := &activityLog{path: filepath.Join(home, activityFile), bus: bus}
	l.load()

	return l
}

func (l *activityLog) load() {
	f, err := os.Open(l.path)
	if err != nil {
		return
	}

	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)

	for sc.Scan() {
		var a Activity

		if json.Unmarshal(sc.Bytes(), &a) == nil {
			l.entries = append(l.entries, a)
		}
	}

	if len(l.entries) > activityKeep {
		l.entries = l.entries[len(l.entries)-activityKeep:]
	}

	l.seq = len(l.entries)
}

func (l *activityLog) add(a Activity) Activity {
	l.mu.Lock()
	l.seq++
	a.At = time.Now()
	a.ID = a.At.UTC().Format("20060102T150405.000Z") + "-" + itoa(l.seq)
	l.entries = append(l.entries, a)

	if len(l.entries) > activityKeep {
		l.entries = l.entries[len(l.entries)-activityKeep:]
	}

	if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		if line, err := json.Marshal(a); err == nil {
			f.Write(append(line, '\n'))
		}

		f.Close()
	}

	l.mu.Unlock()
	l.bus.publish("activity", a)

	return a
}

func (l *activityLog) list(limit int) []Activity {
	l.mu.Lock()
	defer l.mu.Unlock()

	n := len(l.entries)

	if limit <= 0 || limit > n {
		limit = n
	}

	out := make([]Activity, limit)

	for i := 0; i < limit; i++ {
		out[i] = l.entries[n-1-i]
	}

	return out
}

func itoa(n int) string {
	return json.Number(jsonInt(n)).String()
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)

	return string(b)
}
