package syslog

import (
	"errors"
	"net"
	"time"

	"github.com/lynxbase/lynxdb/pkg/event"
)

func (r *Receiver) serveUDP(conn net.PacketConn) error {
	buf := make([]byte, r.currentConfig().MaxMessageBytes+1)
	batch := make([]*event.Event, 0, r.currentConfig().BatchSize)
	timer := newStoppedTimer()
	defer timer.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := r.processBatch(batch); err != nil {
			r.metrics.IncDropped("udp", dropReason(err))
			r.logger.Warn("syslog UDP ingest failed", "error", err, "batch_size", len(batch))
		}
		batch = batch[:0]
		stopTimer(timer)
	}

	for {
		cfg := r.currentConfig()
		if cfg.MaxMessageBytes+1 != len(buf) {
			buf = make([]byte, cfg.MaxMessageBytes+1)
		}
		if udpConn, ok := conn.(*net.UDPConn); ok && cfg.UDPReadBuffer > 0 {
			_ = udpConn.SetReadBuffer(int(cfg.UDPReadBuffer))
		}

		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				select {
				case <-timer.C:
					flush()
				default:
				}
				select {
				case <-r.stop:
					return nil
				default:
					continue
				}
			}
			return err
		}
		if n > cfg.MaxMessageBytes {
			r.metrics.IncDropped("udp", "toolarge")
			continue
		}

		source := ""
		if cfg.UsePeerAsSource && addr != nil {
			source = "udp://" + addr.String()
		}
		e, dialect := newParser(cfg).parse(buf[:n], source, time.Now())
		r.metrics.IncReceived("udp", dialect)
		if e.ParseError {
			r.metrics.IncParseError(dialect)
		}
		if len(batch) == 0 {
			resetTimer(timer, cfg.BatchTimeout.Duration())
		}
		batch = append(batch, e)
		if len(batch) >= cfg.BatchSize {
			flush()
		}

		select {
		case <-timer.C:
			flush()
		default:
		}
	}
}

func dropReason(err error) string {
	if errors.Is(err, errFrameTooLarge) {
		return "toolarge"
	}
	return "backpressure"
}
