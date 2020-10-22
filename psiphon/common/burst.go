/*
 * Copyright (c) 2020, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package common

import (
	"net"
	"sync"
	"time"
)

// BurstMonitoredConn wraps a net.Conn and monitors for data transfer bursts.
// Upstream (read) and downstream (write) bursts are tracked independently.
//
// A burst is defined as a transfer of at least "threshold" bytes, across
// multiple I/O operations where the delay between operations does not exceed
// "deadline". Both a non-zero deadline and theshold must be set to enable
// monitoring. Four bursts are reported: the first, the last, the min (by
// rate) and max.
//
// The reported rates will be more accurate for larger data transfers,
// especially for higher transfer rates. Tune the deadline/threshold as
// required. The threshold should be set to account for buffering (e.g, the
// local host socket send/receive buffer) but this is not enforced by
// BurstMonitoredConn.
//
// Close must be called to complete any outstanding bursts. For complete
// results, call GetMetrics only after Close is called.
//
// Overhead: BurstMonitoredConn adds mutexes but does not use timers.
type BurstMonitoredConn struct {
	net.Conn
	isServer            bool
	readDeadline        time.Duration
	readThresholdBytes  int64
	writeDeadline       time.Duration
	writeThresholdBytes int64

	readMutex        sync.Mutex
	currentReadBurst burst
	readBursts       burstHistory

	writeMutex        sync.Mutex
	currentWriteBurst burst
	writeBursts       burstHistory
}

// NewBurstMonitoredConn creates a new BurstMonitoredConn.
func NewBurstMonitoredConn(
	conn net.Conn,
	isServer bool,
	upstreamDeadline time.Duration,
	upstreamThresholdBytes int64,
	downstreamDeadline time.Duration,
	downstreamThresholdBytes int64) *BurstMonitoredConn {

	burstConn := &BurstMonitoredConn{
		Conn:     conn,
		isServer: isServer,
	}

	if isServer {
		burstConn.readDeadline = upstreamDeadline
		burstConn.readThresholdBytes = upstreamThresholdBytes
		burstConn.writeDeadline = downstreamDeadline
		burstConn.writeThresholdBytes = downstreamThresholdBytes
	} else {
		burstConn.readDeadline = downstreamDeadline
		burstConn.readThresholdBytes = downstreamThresholdBytes
		burstConn.writeDeadline = upstreamDeadline
		burstConn.writeThresholdBytes = upstreamThresholdBytes
	}

	return burstConn
}

type burst struct {
	startTime    time.Time
	lastByteTime time.Time
	bytes        int64
}

func (b *burst) isZero() bool {
	return b.startTime.IsZero()
}

func (b *burst) offset(baseTime time.Time) time.Duration {
	offset := b.startTime.Sub(baseTime)
	if offset <= 0 {
		return 0
	}
	return offset
}

func (b *burst) duration() time.Duration {
	duration := b.lastByteTime.Sub(b.startTime)
	if duration <= 0 {
		return 0
	}
	return duration
}

func (b *burst) rate() int64 {
	return int64(
		(float64(b.bytes) * float64(time.Second)) /
			float64(b.duration()))
}

type burstHistory struct {
	first burst
	last  burst
	min   burst
	max   burst
}

func (conn *BurstMonitoredConn) Read(buffer []byte) (int, error) {

	if conn.readDeadline <= 0 || conn.readThresholdBytes <= 0 {
		return conn.Conn.Read(buffer)
	}

	start := time.Now()
	n, err := conn.Conn.Read(buffer)
	end := time.Now()

	if n > 0 {
		conn.readMutex.Lock()
		conn.updateBurst(
			start,
			end,
			int64(n),
			conn.readDeadline,
			conn.readThresholdBytes,
			&conn.currentReadBurst,
			&conn.readBursts)
		conn.readMutex.Unlock()
	}

	// Note: no context error to preserve error type
	return n, err
}

func (conn *BurstMonitoredConn) Write(buffer []byte) (int, error) {

	if conn.writeDeadline <= 0 || conn.writeThresholdBytes <= 0 {
		return conn.Conn.Write(buffer)
	}

	start := time.Now()
	n, err := conn.Conn.Write(buffer)
	end := time.Now()

	if n > 0 {
		conn.writeMutex.Lock()
		conn.updateBurst(
			start,
			end,
			int64(n),
			conn.writeDeadline,
			conn.writeThresholdBytes,
			&conn.currentWriteBurst,
			&conn.writeBursts)
		conn.writeMutex.Unlock()
	}

	// Note: no context error to preserve error type
	return n, err
}

func (conn *BurstMonitoredConn) Close() error {
	err := conn.Conn.Close()

	if conn.readDeadline > 0 && conn.readThresholdBytes > 0 {
		conn.readMutex.Lock()
		conn.endBurst(
			conn.readThresholdBytes,
			&conn.currentReadBurst,
			&conn.readBursts)
		conn.readMutex.Unlock()
	}

	if conn.writeDeadline > 0 && conn.writeThresholdBytes > 0 {
		conn.writeMutex.Lock()
		conn.endBurst(
			conn.writeThresholdBytes,
			&conn.currentWriteBurst,
			&conn.writeBursts)
		conn.writeMutex.Unlock()
	}

	// Note: no context error to preserve error type
	return err
}

// IsClosed implements the Closer iterface. The return value indicates whether
// the underlying conn has been closed.
func (conn *BurstMonitoredConn) IsClosed() bool {
	closer, ok := conn.Conn.(Closer)
	if !ok {
		return false
	}
	return closer.IsClosed()
}

// GetMetrics returns log fields with burst metrics for the first, last, min
// (by rate), and max bursts for this conn. Time/duration values are reported
// in milliseconds.
func (conn *BurstMonitoredConn) GetMetrics(baseTime time.Time) LogFields {
	logFields := make(LogFields)

	addFields := func(prefix string, burst *burst) {
		if burst.isZero() {
			return
		}
		logFields[prefix+"offset"] = int64(burst.offset(baseTime) / time.Millisecond)
		logFields[prefix+"duration"] = int64(burst.duration() / time.Millisecond)
		logFields[prefix+"bytes"] = burst.bytes
		logFields[prefix+"rate"] = burst.rate()
	}

	addHistory := func(prefix string, history *burstHistory) {
		addFields(prefix+"first_", &history.first)
		addFields(prefix+"last_", &history.last)
		addFields(prefix+"min_", &history.min)
		addFields(prefix+"max_", &history.max)
	}

	var upstreamBursts *burstHistory
	var downstreamBursts *burstHistory

	if conn.isServer {
		upstreamBursts = &conn.readBursts
		downstreamBursts = &conn.writeBursts
	} else {
		upstreamBursts = &conn.writeBursts
		downstreamBursts = &conn.readBursts
	}

	addHistory("burst_upstream_", upstreamBursts)
	addHistory("burst_downstream_", downstreamBursts)

	return logFields
}

func (conn *BurstMonitoredConn) updateBurst(
	operationStart time.Time,
	operationEnd time.Time,
	operationBytes int64,
	deadline time.Duration,
	thresholdBytes int64,
	currentBurst *burst,
	history *burstHistory) {

	// Assumes the associated mutex is locked.

	if currentBurst.isZero() {
		currentBurst.startTime = operationStart
		currentBurst.lastByteTime = operationEnd
		currentBurst.bytes = operationBytes

	} else {

		if operationStart.Sub(currentBurst.lastByteTime) >
			deadline {

			conn.endBurst(thresholdBytes, currentBurst, history)
			currentBurst.startTime = operationStart
		}

		currentBurst.lastByteTime = operationEnd
		currentBurst.bytes += operationBytes
	}

}

func (conn *BurstMonitoredConn) endBurst(
	thresholdBytes int64,
	currentBurst *burst,
	history *burstHistory) {

	// Assumes the associated mutex is locked.

	if currentBurst.isZero() {
		return
	}

	burst := *currentBurst

	currentBurst.startTime = time.Time{}
	currentBurst.lastByteTime = time.Time{}
	currentBurst.bytes = 0

	if burst.bytes < thresholdBytes {
		return
	}

	if history.first.isZero() {
		history.first = burst
	}

	history.last = burst

	if history.min.isZero() || history.min.rate() > burst.rate() {
		history.min = burst
	}

	if history.max.isZero() || history.max.rate() < burst.rate() {
		history.max = burst
	}
}
