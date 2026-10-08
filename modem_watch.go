package main

import (
	"context"
	"log/slog"
	"time"
)

// Liveness probe cadence, OwlShack's. Three misses at 30 s is about 90 s to
// react: slow enough that a busy modem dropping one reply cannot trigger a
// reconnect.
const (
	probeTimeout = 2 * time.Second
	probeMisses  = 3
)

// A var so tests can shrink it.
var probeInterval = 30 * time.Second

// Reconnect backoff, OwlShack's. A radio unplugged for minutes is retried at
// most every 30 s rather than hammered.
const (
	initialRadioRetry = 1 * time.Second
	maxRadioRetry     = 30 * time.Second
)

// watch reports ms on died once the modem stops working, and exits when ms is
// closed. It sends ms itself, not a bare signal: the main loop ignores a report
// about a modem it has already replaced, so a watcher racing a teardown can
// never restart the healthy replacement, and no report is ever dropped.
//
// Two signals, each skipped for a transport that does not offer it:
//   - Dead(): KISS's read loop exited (unplugged, TCP dropped), or the SPI chip
//     stopped re-arming receive. openHop has none: its driver reconnects itself.
//   - A liveness probe, for the case Dead() cannot see: a serial read timeout
//     returns no error, so a board that stays plugged in but goes silent
//     (wedged firmware, a USB suspend that never resumes) leaves the read loop
//     spinning forever. A quiet mesh is normal, so this asks the modem a
//     question instead of waiting for traffic.
func (ms *modemState) watch(died chan<- *modemState) {
	report := func() {
		// Checked first because select picks at random among ready cases, and
		// after a deliberate Close both Dead() and watchDone are closed. Close
		// closes watchDone BEFORE the modem, so by the time a close has fired
		// Dead(), watchDone is already visibly closed and this returns.
		select {
		case <-ms.watchDone:
			return
		default:
		}
		select {
		case died <- ms:
		case <-ms.watchDone:
		}
	}

	if d, ok := ms.modem.(interface{ Dead() <-chan struct{} }); ok {
		dead := d.Dead()
		go func() {
			select {
			case <-dead:
				report()
			case <-ms.watchDone:
			}
		}()
	}

	if lr, ok := ms.stats.(interface{ LastReply() time.Time }); ok {
		// Read here, not in the goroutine, so a test restoring it cannot race a
		// probe that has not started yet.
		interval := probeInterval
		go func() {
			if ms.probe(lr, interval) {
				report()
			}
		}()
	}
}

// probe returns true once the modem has missed probeMisses status queries in a
// row, or false when ms is closed.
func (ms *modemState) probe(lr interface{ LastReply() time.Time }, interval time.Duration) bool {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	misses := 0
	for {
		select {
		case <-ms.watchDone:
			return false
		case <-tick.C:
		}

		before := lr.LastReply()
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		ms.stats.Stats(ctx)
		cancel()

		if before.IsZero() {
			// Asked, but never answered since connecting: this firmware may not
			// implement the queries, and a probe that cannot tell "unsupported"
			// from "dead" would reconnect a working radio forever.
			//
			// ponytail: so a board still silent after one reconnect is not
			// reconnected again; it looks exactly like firmware without the
			// queries. A board wedged through a reopen usually needs power
			// cycling anyway. Upgrade path, if that stops being true: carry
			// "this device has answered before" across a same-config reconnect.
			continue
		}
		if lr.LastReply().After(before) {
			misses = 0
			continue
		}
		misses++
		if misses < probeMisses {
			slog.Warn("modem did not answer a status query",
				"component", "modem", "misses", misses, "limit", probeMisses)
			continue
		}
		slog.Error("modem stopped answering",
			"component", "modem", "silent_for", time.Since(before).Round(time.Second))
		return true
	}
}
