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
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/microbus-io/testarossa"
)

// cached reports whether the cache holds a statement, without counting as a hit.
func (c *queryCache) cached(driverName string, query string) bool {
	_, ok := c.entries.Load(queryKey{driver: driverName, query: query})
	return ok
}

func TestQueryCache_HitReturnsSameEntry(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(64)
	in := "SELECT DATE_ADD_MILLIS(NOW_UTC(), ?) WHERE id=?"

	cold := cache.lookup("mysql", in)
	assert.NoError(cold.err)
	assert.Equal(1, cache.len())

	hot := cache.lookup("mysql", in)
	assert.True(cold == hot)
	assert.Equal(1, cache.len())
}

func TestQueryCache_KeyedOnDriver(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(64)
	in := "SELECT NOW_UTC()"

	mysqlOut := cache.lookup("mysql", in)
	pgxOut := cache.lookup("pgx", in)
	assert.NotEqual(mysqlOut.text, pgxOut.text)
	assert.Equal(2, cache.len())
}

// TestQueryCache_CachesConformedText pins that the cached text is fully unpacked: virtual functions expanded and
// placeholders conformed, with a ? inside a quoted literal left alone.
func TestQueryCache_CachesConformedText(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(64)
	pq := cache.lookup("pgx", "UPDATE t SET at=NOW_UTC(), note='?' WHERE a=? AND b=?")
	assert.NoError(pq.err)
	assert.Equal("UPDATE t SET at=(NOW() AT TIME ZONE 'UTC'), note='?' WHERE a=$1 AND b=$2", pq.text)
}

func TestQueryCache_TelemetryLabels(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(64)
	pq := cache.lookup("pgx", "SELECT a FROM things WHERE id=?")
	assert.Equal("SELECT", pq.op)
	assert.Equal("SELECT things", pq.spanName)

	pq = cache.lookup("pgx", "SELECT a FROM things JOIN others ON things.id=others.id")
	assert.Equal("SELECT", pq.op)
	assert.Equal("SELECT", pq.spanName)
}

func TestQueryCache_ErrorsCached(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(64)
	const in = "SELECT DATE_ADD_MILLIS(only_one)"

	first := cache.lookup("mysql", in)
	assert.Error(first.err)
	assert.Equal(in, first.text)
	assert.Equal(1, cache.len())

	second := cache.lookup("mysql", in)
	assert.True(first == second)
	assert.Equal(1, cache.len())
}

// TestQueryCache_EvictsUnreferenced pins the CLOCK policy: a full cache evicts an entry not hit since the hand
// last passed it, and spares the ones that were.
func TestQueryCache_EvictsUnreferenced(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(3)
	q := func(i int) string { return fmt.Sprintf("SELECT %d", i) }
	for i := range 3 {
		cache.lookup("mysql", q(i))
	}
	assert.Equal(3, cache.len())

	// Hit #0 and #2, so #1 is the only one unreferenced.
	cache.lookup("mysql", q(0))
	cache.lookup("mysql", q(2))

	cache.lookup("mysql", q(3))
	assert.Equal(3, cache.len())
	assert.False(cache.cached("mysql", q(1)))
	for _, i := range []int{0, 2, 3} {
		assert.True(cache.cached("mysql", q(i)), "statement %d", i)
	}
}

// TestQueryCache_HotEntrySurvivesOneOffs pins what the policy is for: a statement the process keeps running
// stays cached through a stream of statements that each run once, where a cache that stops inserting when full
// would have kept whatever arrived first.
func TestQueryCache_HotEntrySurvivesOneOffs(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(4)
	const hot = "SELECT hot"
	for i := range 100 {
		cache.lookup("mysql", hot)
		cache.lookup("mysql", fmt.Sprintf("SELECT %d", i))
	}
	assert.True(cache.cached("mysql", hot))
	assert.True(cache.cached("mysql", "SELECT 99"))
	assert.False(cache.cached("mysql", "SELECT 0"))
	assert.Equal(4, cache.len())
}

func TestQueryCache_DisabledWhenCapacityZero(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(0)
	pq := cache.lookup("mysql", "SELECT NOW_UTC()")
	assert.Equal("SELECT (UTC_TIMESTAMP(3))", pq.text)
	assert.Equal(0, cache.len())

	var nilCache *queryCache
	pq = nilCache.lookup("mysql", "SELECT NOW_UTC()")
	assert.Equal("SELECT (UTC_TIMESTAMP(3))", pq.text)
}

// TestQueryCache_StaleInsertDropped pins that an entry computed before a clear is not inserted after it: it may
// have been unpacked against virtual functions the clear was invalidating.
func TestQueryCache_StaleInsertDropped(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newQueryCache(64)
	generation := cache.generation.Load()
	cache.clear()
	pq := cache.insert(newPreparedQuery("mysql", "SELECT 1"), generation)
	assert.Equal("SELECT 1", pq.text)
	assert.Equal(0, cache.len())
	assert.False(cache.cached("mysql", "SELECT 1"))
}

// TestQueryCache_InvalidatedOnRegister verifies that RegisterVirtualFunc clears the global cache. It cannot run
// in parallel because it mutates the global virtual functions and cache.
func TestQueryCache_InvalidatedOnRegister(t *testing.T) {
	assert := testarossa.For(t)

	// Use a unique virtual function name so other tests are not affected.
	const vfName = "TEST_VF_INVALIDATION_ABCXYZ"
	t.Cleanup(func() { unregisterVirtualFunc(vfName) })

	// A statement naming the function before it exists passes through unexpanded, and is cached that way.
	q, err := unpackQuery("mysql", "SELECT "+vfName+"()")
	assert.NoError(err)
	assert.Equal("SELECT "+vfName+"()", q)
	assert.NotEqual(0, queries.len())

	// Registration must invalidate the cache, so the same statement now expands.
	RegisterVirtualFunc(vfName, func(driverName, args string) (string, error) {
		return "/*test*/", nil
	})
	assert.Equal(0, queries.len())
	q, err = unpackQuery("mysql", "SELECT "+vfName+"()")
	assert.NoError(err)
	assert.Equal("SELECT /*test*/", q)
}

// TestQueryCache_Concurrent runs more distinct statements than the cache holds from many goroutines, so hits,
// inserts and evictions interleave. Run with -race.
func TestQueryCache_Concurrent(t *testing.T) {
	t.Parallel()

	cache := newQueryCache(8)
	const workers = 32
	const iters = 500
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			for i := range iters {
				n := (w + i) % 16
				pq := cache.lookup("pgx", fmt.Sprintf("SELECT NOW_UTC(), ? /* %d */", n))
				want := fmt.Sprintf("SELECT (NOW() AT TIME ZONE 'UTC'), $1 /* %d */", n)
				if pq.err != nil || pq.text != want {
					t.Errorf("got %q, %v; want %q", pq.text, pq.err, want)
					return
				}
			}
		})
	}
	wg.Wait()
	testarossa.For(t).True(cache.len() <= 8)
}

// TestQueryCache_ConcurrentErrorTrace traces the same cached unpacking error from many goroutines. Tracing
// appends to the error's stack, so without the stack clipped the appends would race into one shared array. The
// malformed JSON_FIELD path is traced three deep, which leaves spare capacity in the stack. Run with -race.
func TestQueryCache_ConcurrentErrorTrace(t *testing.T) {
	t.Parallel()

	const in = "SELECT JSON_FIELD(doc, '$.a-b') /* TestQueryCache_ConcurrentErrorTrace */"
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 100 {
				_, err := unpackQuery("mysql", in)
				if err == nil || !strings.Contains(err.Error(), "JSON_FIELD path") {
					t.Errorf("unexpected error: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// BenchmarkQueryCache_Hot measures the cache-hit path.
func BenchmarkQueryCache_Hot(b *testing.B) {
	const q = "UPDATE microbus_steps SET lease_expires=DATE_ADD_MILLIS(NOW_UTC(), ?) WHERE step_id=?"
	cache := newQueryCache(64)
	cache.lookup("pgx", q)
	b.ReportAllocs()
	for b.Loop() {
		if pq := cache.lookup("pgx", q); pq.err != nil {
			b.Fatal(pq.err)
		}
	}
}

// BenchmarkQueryCache_Cold measures the cache-miss path: a unique statement every iteration.
func BenchmarkQueryCache_Cold(b *testing.B) {
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		q := fmt.Sprintf(
			"UPDATE microbus_steps SET lease_expires=DATE_ADD_MILLIS(NOW_UTC(), ?) WHERE step_id=? /* %d */",
			i,
		)
		if pq := newPreparedQuery("pgx", q); pq.err != nil {
			b.Fatal(pq.err)
		}
	}
}
