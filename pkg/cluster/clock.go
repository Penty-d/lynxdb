package cluster

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// ClockProvider abstracts time for testability and simulation.
type ClockProvider interface {
	Now() time.Time
}

// SystemClock returns the real system time.
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// MockClock is a test-controllable clock.
type MockClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewMockClock creates a MockClock set to the given time.
func NewMockClock(t time.Time) *MockClock {
	return &MockClock{now: t}
}

// Now returns the mock clock's current time.
func (m *MockClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.now
}

// Advance moves the clock forward by d.
func (m *MockClock) Advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.now = m.now.Add(d)
}

// Set sets the clock to an absolute time.
func (m *MockClock) Set(t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.now = t
}

// CheckClockSync performs a basic NTP query to estimate local clock offset.
// Warns at >20ms offset, returns error at >100ms offset.
// This is a best-effort check — network issues are logged as warnings, not errors.
func CheckClockSync(logger *slog.Logger) error {
	offset, err := ntpOffset("pool.ntp.org:123")
	if err != nil {
		logger.Warn("clock sync check failed (NTP unreachable)",
			"error", err,
			"suggestion", "ensure UDP port 123 is not blocked")

		return nil // Non-fatal: network may be restricted.
	}

	absOffset := offset
	if absOffset < 0 {
		absOffset = -absOffset
	}

	logger.Info("clock sync check completed",
		"offset", offset,
		"threshold_warn", 20*time.Millisecond,
		"threshold_error", 100*time.Millisecond)

	if absOffset > 100*time.Millisecond {
		return fmt.Errorf("cluster.CheckClockSync: local clock offset %v exceeds 100ms threshold; sync with NTP before joining cluster", offset)
	}
	if absOffset > 20*time.Millisecond {
		logger.Warn("clock offset exceeds 20ms, cluster operations may be unreliable",
			"offset", offset)
	}

	return nil
}

// ntpOffset sends a single NTP query and returns the estimated clock offset.
// Uses the SNTP algorithm: offset = ((t2-t1) + (t3-t4)) / 2
// where t1=client send, t2=server receive, t3=server transmit, t4=client receive.
func ntpOffset(addr string) (time.Duration, error) {
	conn, err := net.DialTimeout("udp", addr, 3*time.Second)
	if err != nil {
		return 0, fmt.Errorf("dial NTP: %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return 0, fmt.Errorf("set deadline: %w", err)
	}

	// NTP packet: 48 bytes, LI=0, VN=4, Mode=3 (client).
	req := make([]byte, 48)
	req[0] = 0x23 // LI=0, VN=4, Mode=3

	t1 := time.Now()

	if _, err := conn.Write(req); err != nil {
		return 0, fmt.Errorf("send NTP request: %w", err)
	}

	resp := make([]byte, 48)
	if _, err := conn.Read(resp); err != nil {
		return 0, fmt.Errorf("read NTP response: %w", err)
	}

	t4 := time.Now()

	// Parse server transmit timestamp (bytes 40-47, NTP epoch 1900-01-01).
	t3 := ntpTimestampToTime(resp[40:48])

	// Parse server receive timestamp (bytes 32-39).
	t2 := ntpTimestampToTime(resp[32:40])

	// SNTP offset: ((t2-t1) + (t3-t4)) / 2
	offset := (t2.Sub(t1) + t3.Sub(t4)) / 2

	return offset, nil
}

// ntpTimestampToTime converts an NTP 64-bit timestamp to time.Time.
// NTP timestamps are seconds since 1900-01-01 (upper 32 bits = seconds,
// lower 32 bits = fractional seconds).
func ntpTimestampToTime(b []byte) time.Time {
	const ntpEpoch = 2208988800 // seconds between 1900 and 1970

	secs := binary.BigEndian.Uint32(b[0:4])
	frac := binary.BigEndian.Uint32(b[4:8])

	// Convert NTP epoch to Unix epoch.
	unixSecs := int64(secs) - ntpEpoch
	nanos := int64(frac) * 1e9 >> 32

	return time.Unix(unixSecs, nanos)
}
