package events

import (
	"fmt"
	"time"
)

// Level classifies an event for display.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

func ms(n int64) string {
	return (time.Duration(n) * time.Millisecond).Round(time.Millisecond).String()
}

// Describe renders an event as a short human-readable line and a level.
func Describe(e Event) (Level, string) {
	switch e.Type {
	case ConnOpen:
		return LevelDebug, fmt.Sprintf(":%d connection from %s", e.Port, e.Remote)
	case ConnClose:
		return LevelDebug, fmt.Sprintf(":%d connection closed (in %dB, out %dB, %s)", e.Port, e.BytesIn, e.BytesOut, ms(e.DurationMs))
	case HTTPRequest:
		lvl := LevelInfo
		if e.Status >= 500 {
			lvl = LevelWarn
		}
		return lvl, fmt.Sprintf(":%d %s %s → %d (%s)", e.Port, e.Method, e.Path, e.Status, ms(e.DurationMs))
	case StartBegin:
		return LevelInfo, fmt.Sprintf("starting (triggered by %s)", e.Trigger)
	case StartOK:
		return LevelInfo, fmt.Sprintf("ready in %s", ms(e.DurationMs))
	case StartFail:
		return LevelError, fmt.Sprintf("start failed after %s: %s", ms(e.DurationMs), e.Error)
	case StopBegin:
		return LevelInfo, fmt.Sprintf("stopping (%s)", e.Reason)
	case StopDone:
		return LevelInfo, fmt.Sprintf("stopped (%s) after running %s", e.Reason, ms(e.DurationMs))
	case ConfigChange:
		return LevelInfo, "configuration applied: " + e.Message
	case Error:
		return LevelError, fmt.Sprintf("%s: %s", e.Message, e.Error)
	case State:
		return LevelDebug, "state → " + e.State
	}
	return LevelInfo, e.Type
}
