package server

import (
	"encoding/json"
	"time"
)

// OptionalTime is a time that may legitimately not have happened yet.
//
// `json:"...,omitempty"` does nothing for a time.Time: omitempty only skips
// empty scalars, and a struct is never empty, so a zero time marshals as
// "0001-01-01T00:00:00Z" and is sent on every response. In JavaScript that is a
// non-empty string, so `if (status.pendingRestartAt)` is true for a server that
// has no pending restart — which is exactly how a freshly added server ended up
// showing a restart banner.
//
// Marshalling zero as null makes the absent case absent.
type OptionalTime struct {
	time.Time
}

// At wraps a time, which may be zero.
func At(t time.Time) OptionalTime { return OptionalTime{t} }

// TimeNow returns the current time as an OptionalTime.
func TimeNow() OptionalTime { return OptionalTime{time.Now()} }

// NoTime is the explicit absent value.
var NoTime = OptionalTime{}

// MarshalJSON writes null when the time is zero.
func (t OptionalTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.Time)
}

// UnmarshalJSON accepts null, an empty string, or an RFC 3339 timestamp.
func (t *OptionalTime) UnmarshalJSON(b []byte) error {
	s := string(b)
	if s == "null" || s == `""` {
		t.Time = time.Time{}
		return nil
	}
	var parsed time.Time
	if err := json.Unmarshal(b, &parsed); err != nil {
		return err
	}
	t.Time = parsed
	return nil
}

// Since is time.Since for an OptionalTime.
func (t OptionalTime) Since() time.Duration { return time.Since(t.Time) }

// Until is time.Until for an OptionalTime.
func (t OptionalTime) Until() time.Duration { return time.Until(t.Time) }
