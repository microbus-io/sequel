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

// callExpand runs expansion using a fresh local cache, so the test stays independent of any
// global cache state and is safe to run in parallel with other tests.
func callExpand(driverName, query string) (string, error) {
	return expandVirtualFuncsWithCache(driverName, query, nil)
}

func TestVF_BasicExpansion(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	q, err := callExpand("mysql", "SELECT NOW_UTC()")
	assert.NoError(err)
	assert.Equal("SELECT (UTC_TIMESTAMP(3))", q)

	q, err = callExpand("pgx", "SELECT NOW_UTC()")
	assert.NoError(err)
	assert.Equal("SELECT (NOW() AT TIME ZONE 'UTC')", q)

	q, err = callExpand("cockroachdb", "SELECT NOW_UTC()")
	assert.NoError(err)
	assert.Equal("SELECT (NOW() AT TIME ZONE 'UTC')", q)

	q, err = callExpand("sqlite", "SELECT NOW_UTC()")
	assert.NoError(err)
	assert.Equal("SELECT (STRFTIME('%Y-%m-%d %H:%M:%f', 'now'))", q)

	q, err = callExpand("mssql", "SELECT NOW_UTC()")
	assert.NoError(err)
	assert.Equal("SELECT (CONVERT(DATETIME2(3), SYSUTCDATETIME()))", q)
}

func TestVF_CaseInsensitive(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	q, err := callExpand("mysql", "SELECT now_utc(), Now_Utc(), NOW_utc()")
	assert.NoError(err)
	assert.Equal("SELECT (UTC_TIMESTAMP(3)), (UTC_TIMESTAMP(3)), (UTC_TIMESTAMP(3))", q)
}

func TestVF_NoExpansionNeeded(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	in := "SELECT id, name FROM users WHERE id=?"
	out, err := callExpand("mysql", in)
	assert.NoError(err)
	assert.Equal(in, out)

	in = "SELECT COUNT(*) FROM users"
	out, err = callExpand("pgx", in)
	assert.NoError(err)
	assert.Equal(in, out)
}

func TestVF_MultipleOccurrencesSamePass(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	q, err := callExpand("mysql", "SELECT NOW_UTC() AS a, NOW_UTC() AS b")
	assert.NoError(err)
	assert.Equal("SELECT (UTC_TIMESTAMP(3)) AS a, (UTC_TIMESTAMP(3)) AS b", q)
}

func TestVF_MultipleDifferentVFsOnePass(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	q, err := callExpand(
		"mysql",
		"SELECT NOW_UTC(), DATE_ADD_MILLIS(created_at, ?), LIMIT_OFFSET(10, 5)",
	)
	assert.NoError(err)
	assert.Equal(
		"SELECT (UTC_TIMESTAMP(3)), DATE_ADD(created_at, INTERVAL (?) * 1000 MICROSECOND), LIMIT 10 OFFSET 5",
		q,
	)
}

func TestVF_NestedVirtualFuncs(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	q, err := callExpand("mysql", "SELECT DATE_ADD_MILLIS(NOW_UTC(), ?)")
	assert.NoError(err)
	assert.Equal("SELECT DATE_ADD((UTC_TIMESTAMP(3)), INTERVAL (?) * 1000 MICROSECOND)", q)

	q, err = callExpand("pgx", "SELECT DATE_DIFF_MILLIS(NOW_UTC(), created_at)")
	assert.NoError(err)
	assert.Equal("SELECT (EXTRACT(EPOCH FROM ((NOW() AT TIME ZONE 'UTC') - created_at)) * 1000.0)", q)
}

func TestVF_QuotedParensAndCommasIgnored(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	q, err := callExpand("mysql", "SELECT DATE_ADD_MILLIS(created_at, ?) WHERE name='hello (world), foo'")
	assert.NoError(err)
	assert.Equal("SELECT DATE_ADD(created_at, INTERVAL (?) * 1000 MICROSECOND) WHERE name='hello (world), foo'", q)

	q, err = callExpand("mysql", "SELECT DATE_ADD_MILLIS(created_at, ?) WHERE name='smile :)'")
	assert.NoError(err)
	assert.Equal("SELECT DATE_ADD(created_at, INTERVAL (?) * 1000 MICROSECOND) WHERE name='smile :)'", q)
}

func TestVF_UnbalancedParensSkippedNotError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Original behavior: unbalanced parens are skipped, not raised as an error.
	in := "SELECT NOW_UTC( WITH no closing"
	out, err := callExpand("mysql", in)
	assert.NoError(err)
	assert.Equal(in, out)
}

func TestVF_NonVFIdentifierLeftAlone(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	in := "SELECT COUNT(*), MAX(price), SUM(qty) FROM orders"
	out, err := callExpand("pgx", in)
	assert.NoError(err)
	assert.Equal(in, out)
}

func TestVF_VFInsideNonVF(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	q, err := callExpand("pgx", "SELECT GREATEST(created_at, NOW_UTC())")
	assert.NoError(err)
	assert.Equal("SELECT GREATEST(created_at, (NOW() AT TIME ZONE 'UTC'))", q)
}

func TestVF_HandlerErrorPropagated(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	_, err := callExpand("mysql", "SELECT DATE_ADD_MILLIS(only_one_arg)")
	assert.Error(err)

	_, err = callExpand("mysql", "SELECT REGEXP_TEXT_SEARCH(? without_in_keyword)")
	assert.Error(err)

	_, err = callExpand("oracle", "SELECT NOW_UTC()")
	assert.Error(err)
}

func TestVF_CacheHitReturnsSameValue(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newVFCache(64)
	in := "SELECT DATE_ADD_MILLIS(NOW_UTC(), ?) WHERE id=?"

	cold, err := expandVirtualFuncsWithCache("mysql", in, cache)
	assert.NoError(err)
	assert.Equal(1, cache.len())

	hot, err := expandVirtualFuncsWithCache("mysql", in, cache)
	assert.NoError(err)
	assert.Equal(cold, hot)
	assert.Equal(1, cache.len())
}

func TestVF_CacheKeyedOnDriver(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newVFCache(64)
	in := "SELECT NOW_UTC()"

	mysqlOut, err := expandVirtualFuncsWithCache("mysql", in, cache)
	assert.NoError(err)
	pgxOut, err := expandVirtualFuncsWithCache("pgx", in, cache)
	assert.NoError(err)
	assert.NotEqual(mysqlOut, pgxOut)
	assert.Equal(2, cache.len())
}

func TestVF_CacheErrorsToo(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newVFCache(64)

	_, err := expandVirtualFuncsWithCache("mysql", "SELECT DATE_ADD_MILLIS(only_one)", cache)
	assert.Error(err)
	first := err.Error()
	assert.Equal(1, cache.len())

	_, err = expandVirtualFuncsWithCache("mysql", "SELECT DATE_ADD_MILLIS(only_one)", cache)
	assert.Error(err)
	assert.Equal(first, err.Error())
	assert.Equal(1, cache.len())
}

func TestVF_CacheLRUEvictsOldest(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newVFCache(3)

	for i := 0; i < 3; i++ {
		q := fmt.Sprintf("SELECT NOW_UTC() /* %d */", i)
		_, err := expandVirtualFuncsWithCache("mysql", q, cache)
		assert.NoError(err)
	}
	assert.Equal(3, cache.len())

	// Touch query #0 so #1 becomes the LRU.
	_, err := expandVirtualFuncsWithCache("mysql", "SELECT NOW_UTC() /* 0 */", cache)
	assert.NoError(err)

	// Insert a fourth, evicting query #1.
	_, err = expandVirtualFuncsWithCache("mysql", "SELECT NOW_UTC() /* 3 */", cache)
	assert.NoError(err)
	assert.Equal(3, cache.len())

	_, evictedHit := cache.get(vfCacheKey{driver: "mysql", query: "SELECT NOW_UTC() /* 1 */"})
	assert.False(evictedHit)
	for _, idx := range []int{0, 2, 3} {
		_, ok := cache.get(vfCacheKey{
			driver: "mysql",
			query:  fmt.Sprintf("SELECT NOW_UTC() /* %d */", idx),
		})
		assert.True(ok)
	}
}

func TestVF_CacheDisabledWhenCapacityZero(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	cache := newVFCache(0)
	_, err := expandVirtualFuncsWithCache("mysql", "SELECT NOW_UTC()", cache)
	assert.NoError(err)
	assert.Equal(0, cache.len())

	q, err := expandVirtualFuncsWithCache("mysql", "SELECT NOW_UTC()", cache)
	assert.NoError(err)
	assert.Equal("SELECT (UTC_TIMESTAMP(3))", q)
	assert.Equal(0, cache.len())
}

func TestVF_NilCacheDisablesCache(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	q, err := expandVirtualFuncsWithCache("mysql", "SELECT NOW_UTC()", nil)
	assert.NoError(err)
	assert.Equal("SELECT (UTC_TIMESTAMP(3))", q)
}

// TestVF_CacheInvalidatedOnRegister verifies that RegisterVirtualFunc clears the global cache.
// This test cannot run in parallel because it mutates the global VF map and cache.
func TestVF_CacheInvalidatedOnRegister(t *testing.T) {
	assert := testarossa.For(t)

	// Use a unique VF name so other tests are not affected.
	const vfName = "TEST_VF_INVALIDATION_ABCXYZ"
	upper := strings.ToUpper(vfName)

	t.Cleanup(func() {
		virtualFuncsMutex.Lock()
		delete(virtualFuncsMap, upper)
		virtualFuncsMutex.Unlock()
		expandCache.clear()
	})

	// Prime the global cache with at least one entry.
	_, err := expandVirtualFuncs("mysql", "SELECT NOW_UTC() /* TestVF_CacheInvalidatedOnRegister */")
	assert.NoError(err)
	assert.NotEqual(0, expandCache.len())

	// Registration must invalidate the cache.
	RegisterVirtualFunc(vfName, func(driverName, args string) (string, error) {
		return "/*test*/", nil
	})
	assert.Equal(0, expandCache.len())

	// And the freshly registered VF is picked up.
	q, err := expandVirtualFuncs("mysql", "SELECT "+vfName+"()")
	assert.NoError(err)
	assert.Equal("SELECT /*test*/", q)
}

func TestVF_NewlyRegisteredVFExpanded(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	const vfName = "TEST_VF_DOUBLE_XYZ"
	upper := strings.ToUpper(vfName)
	t.Cleanup(func() {
		virtualFuncsMutex.Lock()
		delete(virtualFuncsMap, upper)
		virtualFuncsMutex.Unlock()
		expandCache.clear()
	})

	RegisterVirtualFunc(vfName, func(driverName, args string) (string, error) {
		return "DOUBLE(" + args + ")", nil
	})

	q, err := callExpand("mysql", "SELECT "+vfName+"(price) FROM t")
	assert.NoError(err)
	assert.Equal("SELECT DOUBLE(price) FROM t", q)
}

func TestVF_ConcurrentCallsAreSafe(t *testing.T) {
	t.Parallel()

	cache := newVFCache(256)
	const workers = 32
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				q := fmt.Sprintf("SELECT NOW_UTC() /* w=%d i=%d */", w%4, i%4)
				out, err := expandVirtualFuncsWithCache("mysql", q, cache)
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				if !strings.Contains(out, "(UTC_TIMESTAMP(3))") {
					t.Errorf("unexpected result: %q", out)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

func TestVF_NoMatchReturnsSameString(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	in := "SELECT 1 + 1"
	out, err := callExpand("mysql", in)
	assert.NoError(err)
	assert.Equal(in, out)
}

// BenchmarkExpandVirtualFuncs_Hot measures the cache-hit path.
func BenchmarkExpandVirtualFuncs_Hot(b *testing.B) {
	const q = "UPDATE microbus_steps SET lease_expires=DATE_ADD_MILLIS(NOW_UTC(), ?) WHERE step_id=?"
	cache := newVFCache(64)
	if _, err := expandVirtualFuncsWithCache("pgx", q, cache); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := expandVirtualFuncsWithCache("pgx", q, cache); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkExpandVirtualFuncs_Cold measures the cache-miss path: a unique query every iteration.
func BenchmarkExpandVirtualFuncs_Cold(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := fmt.Sprintf(
			"UPDATE microbus_steps SET lease_expires=DATE_ADD_MILLIS(NOW_UTC(), ?) WHERE step_id=? /* %d */",
			i,
		)
		if _, err := expandVirtualFuncsWithCache("pgx", q, nil); err != nil {
			b.Fatal(err)
		}
	}
}
