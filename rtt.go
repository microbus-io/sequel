/*
Copyright (c) 2025-2026 Microbus LLC and various contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sequel

import (
	"context"
	"time"
)

/*
SimulateRTT makes every operation sequel sends over the wire pause for the given duration first,
simulating the round-trip latency of a remote server. It is a testing aid: against in-memory SQLite or a
server on localhost a round trip costs microseconds, so needlessly chatty code performs the same as code
that batches, and timeout paths never fire. Raising the round trip to what a real network costs makes both
visible in a test.

The delay is charged per round trip, not per call: a [DB.Transact] that begins a transaction, runs three
statements and commits pays it five times (six on SQL Server, which adds a SET XACT_ABORT ON preamble).
It applies to the statement methods (Exec, Query, QueryRow, Prepare, and the Context variants) on [DB] and
[Tx], to each execution of a prepared [Stmt], to Begin/BeginTx, Commit and Rollback, and to Ping.
[DB.InsertReturnID] is one statement on every driver, so it pays once.

Three things are not charged. A [sql.Conn] talks to the driver directly, with sequel out of the path.
Fetching successive rows from an open [Rows] is batched by the driver, so a full round trip per Next would
model the wire worse than nothing. Lifecycle — Close on a pool or a [Stmt], and the DROP that retires a
testing database — is not caller-facing work.

The Context variants honor their context: a deadline shorter than the simulated latency fails the
operation with the context's error and never reaches the database, as a real round trip outliving its
deadline does.

Zero turns the simulation off and is the default; a negative duration is treated as zero. The setting is
safe to change while the pool is in use and applies to operations begun after it. A [Tx] captures it at
begin, so one transaction runs at one latency. For a *DB shared by [OpenSingleton] it is process-wide for
that pool — last writer wins — so set it from the owning caller.

This is deliberate latency injection and slows real work exactly as advertised. Keep it behind the same
switch that selects your test database.
*/
func (db *DB) SimulateRTT(delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	db.simulatedRTT.Store(int64(delay))
}

// rtt is the currently configured simulated round-trip time, zero when the simulation is off.
func (db *DB) rtt() time.Duration {
	return time.Duration(db.simulatedRTT.Load())
}

// simulateRTT pauses before an operation reaches the database, reporting the context's error if it is
// cancelled first. A zero delay returns immediately, keeping the un-simulated path to one comparison.
func simulateRTT(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
