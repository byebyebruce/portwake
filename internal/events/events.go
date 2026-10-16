// Package events is the in-process event bus shared by store, web, tui and logs.
package events

import (
	"sync"
	"time"
)

// Event types.
const (
	ConnOpen     = "conn_open"
	ConnClose    = "conn_close"
	StartBegin   = "start_begin"
	StartOK      = "start_ok"
	StartFail    = "start_fail"
	StopBegin    = "stop_begin"
	StopDone     = "stop_done"
	HTTPRequest  = "http_request"
	ConfigChange = "config_change"
	Error        = "error"
	// State is a transient state-transition notification; it is not persisted.
	State = "state"
)

// TriggerManual is the start trigger for manual starts (web / TUI).
const TriggerManual = "manual"

// Stop reasons.
const (
	ReasonIdle     = "idle"
	ReasonManual   = "manual"
	ReasonExternal = "external"
)

type Event struct {
	ID         int64     `json:"id,omitempty"`
	Time       time.Time `json:"time"`
	Project    string    `json:"project,omitempty"`
	Port       int       `json:"port,omitempty"`
	Type       string    `json:"type"`
	Message    string    `json:"message,omitempty"`
	State      string    `json:"state,omitempty"`
	Trigger    string    `json:"trigger,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Error      string    `json:"error,omitempty"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	BytesIn    int64     `json:"bytes_in,omitempty"`
	BytesOut   int64     `json:"bytes_out,omitempty"`
	Remote     string    `json:"remote,omitempty"`
	Method     string    `json:"method,omitempty"`
	Path       string    `json:"path,omitempty"`
	Status     int       `json:"status,omitempty"`
}

// Bus fans events out to subscribers. Slow subscribers drop events.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

func (b *Bus) Publish(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns a channel of events and a cancel func that closes it.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	ch := make(chan Event, buffer)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
}
