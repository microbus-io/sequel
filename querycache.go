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
	"slices"
	"sync"
	"sync/atomic"

	"github.com/microbus-io/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// queryCacheSize bounds the number of statements retained in the query cache.
const queryCacheSize = 4096

// queries caches every statement sequel runs, by driver and text.
var queries = newQueryCache(queryCacheSize)

// queryKey identifies a statement: the same text unpacks differently per driver.
type queryKey struct {
	driver string
	query  string
}

/*
preparedQuery is everything about a statement that depends only on its driver and text: the unpacked text that
is executed, and the labels and option slices its telemetry reports with. It is computed once, on a cache miss,
and is immutable after; the option slices are shared by every execution and must never be appended to or
modified.
*/
type preparedQuery struct {
	key queryKey
	// text is the unpacked statement, or the original one when unpacking failed.
	text string
	// err is the unpacking error. Its stack is clipped, so tracing it again on concurrent executions does not
	// append into a shared array.
	err error

	op       string
	spanName string
	// spanOptions start a span: the client kind and the db.* attributes.
	spanOptions []trace.SpanStartOption
	// recordOK and recordFailed carry the attributes of a sequel_query_duration sample by status.
	recordOK     []metric.RecordOption
	recordFailed []metric.RecordOption
	// addContention carries the attributes of a sequel_lock_contention increment.
	addContention []metric.AddOption

	// referenced is set by a hit and cleared by the eviction hand passing over the entry.
	referenced atomic.Bool
}

// newPreparedQuery unpacks a statement and derives its telemetry labels.
func newPreparedQuery(driverName string, query string) *preparedQuery {
	pq := &preparedQuery{key: queryKey{driver: driverName, query: query}}
	expanded, err := expandVirtualFuncs(driverName, query)
	if err != nil {
		if te, ok := err.(*errors.TracedError); ok {
			te.Stack = slices.Clip(te.Stack)
		}
		pq.text = query
		pq.err = err
	} else {
		pq.text = conformPlaceholders(driverName, expanded)
	}

	op, table := parseOperation(pq.text)
	pq.op = op
	pq.spanName = op
	attrs := []attribute.KeyValue{
		attribute.String("db.system.name", driverName),
		attribute.String("db.operation.name", op),
	}
	if table != "" {
		pq.spanName = op + " " + table
		attrs = append(attrs, attribute.String("db.collection.name", table))
	}
	pq.spanOptions = []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	}
	system := attribute.String("db.system.name", driverName)
	operation := attribute.String("db.operation.name", op)
	pq.recordOK = []metric.RecordOption{
		metric.WithAttributeSet(attribute.NewSet(system, operation, attribute.String("status", "ok"))),
	}
	pq.recordFailed = []metric.RecordOption{
		metric.WithAttributeSet(attribute.NewSet(system, operation, attribute.String("status", "error"))),
	}
	pq.addContention = []metric.AddOption{
		metric.WithAttributeSet(attribute.NewSet(system, operation)),
	}
	return pq
}

/*
queryCache maps a statement to its [preparedQuery]. A hit is lock-free: a sync.Map load, and an atomic flag set
only if it is not set already, so the statements a process keeps running are read without writing to shared
memory. A miss computes the entry and inserts it under the mutex, evicting by CLOCK: the hand sweeps the ring,
clearing the flag of each entry it passes, and evicts the first entry whose flag is already clear - one not hit
since the hand last passed it.
*/
type queryCache struct {
	capacity int
	entries  sync.Map // queryKey -> *preparedQuery

	mutex sync.Mutex // guards ring and hand, and orders inserts against clear
	ring  []*preparedQuery
	hand  int
	// generation is bumped by clear, so an entry computed against the virtual functions registered before it
	// is not inserted after it.
	generation atomic.Uint64
}

// newQueryCache returns a cache holding up to capacity statements. A capacity of zero disables caching.
func newQueryCache(capacity int) *queryCache {
	return &queryCache{capacity: max(capacity, 0)}
}

// lookup returns the prepared form of a statement, computing it on a miss. A nil cache computes it every time.
func (c *queryCache) lookup(driverName string, query string) *preparedQuery {
	if c == nil || c.capacity == 0 {
		return newPreparedQuery(driverName, query)
	}
	if v, ok := c.entries.Load(queryKey{driver: driverName, query: query}); ok {
		pq := v.(*preparedQuery)
		if !pq.referenced.Load() {
			pq.referenced.Store(true)
		}
		return pq
	}
	generation := c.generation.Load()
	return c.insert(newPreparedQuery(driverName, query), generation)
}

// insert adds a freshly computed entry, evicting one if the cache is full, and returns the entry to use: an
// equal one another goroutine inserted first wins.
func (c *queryCache) insert(pq *preparedQuery, generation uint64) *preparedQuery {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.generation.Load() != generation {
		return pq
	}
	if v, ok := c.entries.Load(pq.key); ok {
		return v.(*preparedQuery)
	}
	if len(c.ring) < c.capacity {
		c.ring = append(c.ring, pq)
	} else {
		// Hits re-mark entries behind the hand without the lock, so the sweep is bounded: after two full turns
		// the entry under the hand goes regardless.
		for range 2 * c.capacity {
			if !c.ring[c.hand].referenced.Load() {
				break
			}
			c.ring[c.hand].referenced.Store(false)
			c.hand = (c.hand + 1) % c.capacity
		}
		c.entries.Delete(c.ring[c.hand].key)
		c.ring[c.hand] = pq
		c.hand = (c.hand + 1) % c.capacity
	}
	c.entries.Store(pq.key, pq)
	return pq
}

// clear drops every entry.
func (c *queryCache) clear() {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.generation.Add(1)
	c.entries.Clear()
	clear(c.ring)
	c.ring = c.ring[:0]
	c.hand = 0
}

// len returns the number of cached statements.
func (c *queryCache) len() int {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return len(c.ring)
}

// lookupQuery returns the prepared form of a statement from the process-wide cache.
func lookupQuery(driverName string, query string) *preparedQuery {
	return queries.lookup(driverName, query)
}
