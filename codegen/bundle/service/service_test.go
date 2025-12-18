package service

import (
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microbus-io/sequel/codegen/bundle/application"
	"github.com/microbus-io/sequel/codegen/bundle/connector"
	"github.com/microbus-io/sequel/codegen/bundle/service/serviceapi"
	"github.com/microbus-io/testarossa"
)

func TestService_Create(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.create.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("key_increments", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			keys[i], err = client.Create(ctx, NewObject(t))
			assert.NoError(err)
			if i > 0 {
				assert.True(keys[i].ID > keys[i-1].ID)
			}
		}
	})

	t.Run("nil_input", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, nil)
		assert.Error(err)
		assert.Zero(key)
	})

	t.Run("invalid_input", func(t *testing.T) {
		assert := testarossa.For(t)

		invalidObj := NewObject(t)
		invalidObj.Example = strings.Repeat("X", 1024) // Too long
		assert.Error(invalidObj.Validate(ctx))
		key, err := client.Create(ctx, invalidObj)
		assert.Error(err)
		assert.Zero(key)
	})

	t.Run("non_zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Key = serviceapi.ParseKey(999999)
		ignoredKey := serviceapi.ParseKey(999999)
		key, err := client.Create(ctx, newObj)
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		assert.NotEqual(ignoredKey, key)
	})

	t.Run("concurrently", func(t *testing.T) {
		assert := testarossa.For(t)

		var wg sync.WaitGroup
		n := 10
		wg.Add(n)
		for range n {
			go func() {
				defer wg.Done()
				key, err := client.Create(ctx, NewObject(t))
				assert.Expect(
					key.IsZero(), false,
					err, nil,
				)
			}()
		}
		wg.Wait()
	})

	t.Run("max_example_value", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Example = strings.Repeat("X", 256) // Max length allowed
		key, err := client.Create(ctx, newObj)
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			found, true,
			err, nil,
		)
	})

	t.Run("timestamps_set_on_create", func(t *testing.T) {
		assert := testarossa.For(t)

		before := time.Now().UTC().Add(-time.Second)
		key, err := client.Create(ctx, NewObject(t))
		after := time.Now().UTC().Add(time.Second)
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			found, true,
			err, nil,
		)
		assert.False(obj.CreatedAt.Before(before))
		assert.False(obj.CreatedAt.After(after))
		assert.False(obj.UpdatedAt.Before(before))
		assert.False(obj.UpdatedAt.After(after))
	})
}

func TestService_Store(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.store.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("create_and_store", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Example = "Original"
		key, err := client.Create(ctx, newObj)
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj != nil, true,
			originalObj.Key, key,
			found, true,
			err, nil,
		)
		assert.Expect(
			// HINT: Validate other fields of the original object here
			originalObj.Example, newObj.Example,
		)

		// HINT: Modify the fields of the original object here
		originalObj.Example = "Modified"

		storedObj, err := client.Store(ctx, originalObj)
		assert.Expect(
			err, nil,
			storedObj, true,
		)

		modifiedObj, found, err := client.Load(ctx, key)
		assert.Expect(
			modifiedObj != nil, true,
			modifiedObj.Key, key,
			found, true,
			err, nil,
		)
		assert.Expect(
			// HINT: Validate more fields of the modified object here, in particular those that were changed above
			modifiedObj.Example, originalObj.Example,
		)
	})

	t.Run("nil_input", func(t *testing.T) {
		assert := testarossa.For(t)

		stored, err := client.Store(ctx, nil)
		assert.Error(err)
		assert.False(stored)
	})

	t.Run("invalid_input", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, objKey)
		assert.Expect(
			originalObj != nil, true,
			originalObj.Key, objKey,
			found, true,
			err, nil,
		)

		invalidObj := *originalObj
		invalidObj.Example = strings.Repeat("X", 1024) // Too long
		assert.Error(invalidObj.Validate(ctx))
		stored, err := client.Store(ctx, &invalidObj)
		assert.Error(err)
		assert.False(stored)
	})

	t.Run("store_after_delete", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj == nil, false,
			found, true,
			err, nil,
		)
		stored, err := client.Store(ctx, originalObj)
		assert.Expect(
			err, nil,
			stored, true,
		)
		deleted, err := client.Delete(ctx, originalObj.Key)
		assert.Expect(
			err, nil,
			deleted, true,
		)
		stored, err = client.Store(ctx, originalObj)
		assert.Expect(
			err, nil,
			stored, false,
		)
	})

	t.Run("store_with_zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj == nil, false,
			found, true,
			err, nil,
		)
		originalObj.Key = serviceapi.ObjKey{}
		stored, err := client.Store(ctx, originalObj)
		assert.Error(err)
		assert.False(stored)
	})

	t.Run("store_twice", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj != nil, true,
			originalObj.Key, key,
			found, true,
			err, nil,
		)
		stored, err := client.Store(ctx, originalObj)
		assert.Expect(
			err, nil,
			stored, true,
		)
		stored, err = client.Store(ctx, originalObj)
		assert.Expect(
			err, nil,
			stored, true,
		)
	})

	t.Run("concurrently", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj != nil, true,
			found, true,
			err, nil,
		)

		var wg sync.WaitGroup
		n := 10
		wg.Add(n)
		for i := range n {
			go func() {
				defer wg.Done()
				obj := *originalObj
				obj.Example = strconv.Itoa(i + 1)
				stored, err := client.Store(ctx, &obj)
				assert.Expect(
					err, nil,
					stored, true,
				)
			}()
		}
		wg.Wait()

		updatedObj, found, err := client.Load(ctx, key)
		ex, _ := strconv.Atoi(updatedObj.Example)
		assert.Expect(
			updatedObj != nil, true,
			ex >= 1 && ex <= n, true,
			found, true,
			err, nil,
		)
	})

	t.Run("multiple_updates", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)

		n := 5
		for i := range n {
			obj, found, err := client.Load(ctx, key)
			assert.Expect(
				obj != nil, true,
				found, true,
				err, nil,
			)
			obj.Example = strconv.Itoa(i + 1)
			stored, err := client.Store(ctx, obj)
			assert.Expect(
				err, nil,
				stored, true,
			)
		}

		finalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			finalObj != nil, true,
			finalObj.Example, strconv.Itoa(n),
			found, true,
			err, nil,
		)
	})

	t.Run("timestamps_updated_on_store", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj != nil, true,
			found, true,
			err, nil,
		)
		originalCreatedAt := originalObj.CreatedAt
		originalUpdatedAt := originalObj.UpdatedAt

		time.Sleep(10 * time.Millisecond) // Ensure time has passed

		originalObj.Example = "Modified"
		before := time.Now().UTC().Add(-time.Second)
		stored, err := client.Store(ctx, originalObj)
		after := time.Now().UTC().Add(time.Second)
		assert.Expect(
			err, nil,
			stored, true,
		)

		modifiedObj, found, err := client.Load(ctx, key)
		assert.Expect(
			modifiedObj != nil, true,
			found, true,
			err, nil,
		)
		// CreatedAt should remain unchanged
		assert.Equal(originalCreatedAt.UTC(), modifiedObj.CreatedAt.UTC())
		// UpdatedAt should be updated
		assert.True(modifiedObj.UpdatedAt.After(originalUpdatedAt))
		assert.False(modifiedObj.UpdatedAt.Before(before))
		assert.False(modifiedObj.UpdatedAt.After(after))
	})
}

func TestService_MustStore(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.muststore.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("create_store_delete", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		objKey, err := client.Create(ctx, newObj)
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)
		obj, err := client.MustLoad(ctx, objKey)
		assert.NoError(err)
		assert.NotNil(obj)
		err = client.MustStore(ctx, obj)
		assert.NoError(err)
		deleted, err := client.Delete(ctx, objKey)
		assert.Expect(
			err, nil,
			deleted, true,
		)
		err = client.MustStore(ctx, obj)
		assert.Error(err)
	})

	t.Run("store_zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		err := client.MustStore(ctx, &serviceapi.Obj{})
		assert.Error(err)
	})

	t.Run("store_non_existent_key", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Key = serviceapi.ParseKey(999999)
		err := client.MustStore(ctx, newObj)
		assert.Error(err)
	})

	t.Run("nil_input", func(t *testing.T) {
		assert := testarossa.For(t)

		err := client.MustStore(ctx, nil)
		assert.Error(err)
	})
}

func TestService_Revise(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.revise.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("revise_matching_revision", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Example = "Original"
		key, err := client.Create(ctx, newObj)
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj != nil, true,
			found, true,
			err, nil,
		)
		originalObj.Example = "Modified"
		revised, err := client.Revise(ctx, originalObj)
		assert.Expect(
			err, nil,
			revised, true,
		)
		modifiedObj, found, err := client.Load(ctx, key)
		assert.Expect(
			modifiedObj != nil, true,
			modifiedObj.Example, "Modified",
			found, true,
			err, nil,
		)
	})

	t.Run("revise_stale_revision", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		key, err := client.Create(ctx, newObj)
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj != nil, true,
			found, true,
			err, nil,
		)
		// Store to bump the revision
		originalObj.Example = "First"
		stored, err := client.Store(ctx, originalObj)
		assert.Expect(
			err, nil,
			stored, true,
		)
		// Revise with stale revision should fail
		originalObj.Example = "Second"
		revised, err := client.Revise(ctx, originalObj)
		assert.Expect(
			err, nil,
			revised, false,
		)
	})

	t.Run("nil_input", func(t *testing.T) {
		assert := testarossa.For(t)

		revised, err := client.Revise(ctx, nil)
		assert.Error(err)
		assert.False(revised)
	})

	t.Run("zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		revised, err := client.Revise(ctx, &serviceapi.Obj{})
		assert.Error(err)
		assert.False(revised)
	})

	t.Run("nonexistent_key", func(t *testing.T) {
		assert := testarossa.For(t)

		obj := NewObject(t)
		obj.Key = serviceapi.ParseKey(999999)
		obj.Revision = 1
		revised, err := client.Revise(ctx, obj)
		assert.Expect(
			err, nil,
			revised, false,
		)
	})

	t.Run("invalid_input", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, objKey)
		assert.Expect(
			originalObj != nil, true,
			found, true,
			err, nil,
		)

		invalidObj := *originalObj
		invalidObj.Example = strings.Repeat("X", 1024) // Too long
		assert.Error(invalidObj.Validate(ctx))
		revised, err := client.Revise(ctx, &invalidObj)
		assert.Error(err)
		assert.False(revised)
	})

	t.Run("concurrent_same_object", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		originalObj, found, err := client.Load(ctx, key)
		assert.Expect(
			originalObj != nil, true,
			found, true,
			err, nil,
		)

		var wg sync.WaitGroup
		n := 5
		wg.Add(n)
		revised := make([]bool, n)
		for i := range n {
			go func() {
				defer wg.Done()
				obj := *originalObj
				obj.Example = strconv.Itoa(i + 1)
				var err error
				revised[i], err = client.Revise(ctx, &obj)
				assert.NoError(err)
			}()
		}
		wg.Wait()

		// Exactly one goroutine should have succeeded due to optimistic locking
		successCount := 0
		for i := range n {
			if revised[i] {
				successCount++
			}
		}
		assert.Equal(1, successCount)
	})
}

func TestService_MustRevise(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.mustrevise.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("matching_revision", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		key, err := client.Create(ctx, newObj)
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, err := client.MustLoad(ctx, key)
		assert.NoError(err)
		assert.NotNil(obj)
		err = client.MustRevise(ctx, obj)
		assert.NoError(err)
	})

	t.Run("stale_revision", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		key, err := client.Create(ctx, newObj)
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		original, err := client.MustLoad(ctx, key)
		assert.NoError(err)
		// Store to bump the revision
		stored, err := client.Store(ctx, original)
		assert.Expect(
			err, nil,
			stored, true,
		)
		// MustRevise with stale revision should error
		err = client.MustRevise(ctx, original)
		assert.Error(err)
	})

	t.Run("nonexistent_key", func(t *testing.T) {
		assert := testarossa.For(t)

		obj := NewObject(t)
		obj.Key = serviceapi.ParseKey(999999)
		obj.Revision = 1
		err := client.MustRevise(ctx, obj)
		assert.Error(err)
	})

	t.Run("nil_input", func(t *testing.T) {
		assert := testarossa.For(t)

		err := client.MustRevise(ctx, nil)
		assert.Error(err)
	})

	t.Run("zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		err := client.MustRevise(ctx, &serviceapi.Obj{})
		assert.Error(err)
	})
}

func TestService_Delete(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.delete.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("create_and_delete", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		createdObj, found, err := client.Load(ctx, key)
		assert.Expect(
			createdObj == nil, false,
			found, true,
			err, nil,
		)
		deleted, err := client.Delete(ctx, key)
		assert.Expect(
			err, nil,
			deleted, true,
		)
		deletedObj, found, err := client.Load(ctx, key)
		assert.Expect(
			deletedObj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("delete_zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		deleted, err := client.Delete(ctx, serviceapi.ObjKey{})
		assert.Expect(
			err, nil,
			deleted, false,
		)
	})

	t.Run("delete_non_existent_key", func(t *testing.T) {
		assert := testarossa.For(t)

		deleted, err := client.Delete(ctx, serviceapi.ParseKey(999999))
		assert.Expect(
			err, nil,
			deleted, false,
		)
	})

	t.Run("delete_twice", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)
		deleted, err := client.Delete(ctx, objKey)
		assert.Expect(
			err, nil,
			deleted, true,
		)
		deleted, err = client.Delete(ctx, objKey)
		assert.Expect(
			err, nil,
			deleted, false,
		)
	})

	t.Run("concurrently", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 10
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			keys[i], err = client.Create(ctx, NewObject(t))
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		var wg sync.WaitGroup
		wg.Add(n)
		for i := range n {
			go func() {
				defer wg.Done()
				deleted, err := client.Delete(ctx, keys[i])
				assert.Expect(
					err, nil,
					deleted, true,
				)
			}()
		}
		wg.Wait()

		for i := range n {
			obj, found, err := client.Load(ctx, keys[i])
			assert.Expect(
				obj, nil,
				found, false,
				err, nil,
			)
		}
	})

	t.Run("same_key_concurrently", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		var wg sync.WaitGroup
		n := 5
		wg.Add(n)
		for range n {
			go func() {
				defer wg.Done()
				_, err := client.Delete(ctx, objKey)
				assert.NoError(err)
			}()
		}
		wg.Wait()

		obj, found, err := client.Load(ctx, objKey)
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})
}

func TestService_MustDelete(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.mustdelete.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("delete_twice", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		objKey, err := client.Create(ctx, newObj)
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)
		err = client.MustDelete(ctx, objKey)
		assert.NoError(err)
		err = client.MustDelete(ctx, objKey)
		assert.Error(err)
	})

	t.Run("delete_zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		err := client.MustDelete(ctx, serviceapi.ObjKey{})
		assert.Error(err)
	})

	t.Run("delete_non_existent_key", func(t *testing.T) {
		assert := testarossa.For(t)

		err := client.MustDelete(ctx, serviceapi.ParseKey(999999))
		assert.Error(err)
	})
}

func TestService_List(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.list.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("offset_limit_total_count", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 10
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			newObj := NewObject(t)
			newObj.Example = t.Name()
			keys[i], err = client.Create(ctx, newObj)
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		objs, totalCount, err := client.List(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(totalCount, n, err, nil)
		if assert.Len(objs, n) {
			for i := range n {
				if !assert.Equal(keys[i], objs[i].Key) {
					break
				}
			}
		}

		objs, _, err = client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			Offset:  1,
			Limit:   n / 2,
		})
		assert.Expect(totalCount, n, err, nil)
		if assert.Len(objs, n/2) {
			for i := 1; i < 1+n/2; i++ {
				if !assert.Equal(keys[i], objs[i-1].Key) {
					break
				}
			}
		}
	})

	t.Run("order_by", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 10
		keys := make([]serviceapi.ObjKey, n)
		for i := range 10 {
			var err error
			newObj := NewObject(t)
			newObj.Example = t.Name()
			keys[i], err = client.Create(ctx, newObj)
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		// Default order is by ID ascending
		objs, _, err := client.List(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				if !assert.Equal(keys[i], objs[i].Key) {
					break
				}
			}
		}

		// Sort by ID ascending
		objs, _, err = client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			OrderBy: "id",
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				if !assert.Equal(keys[i], objs[i].Key) {
					break
				}
			}
		}

		// Sort by non-ID ascending
		objs, _, err = client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			OrderBy: "example",
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				if !assert.Equal(keys[i], objs[i].Key) {
					break
				}
			}
		}

		// Sort by ID descending
		objs, _, err = client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			OrderBy: "-id",
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				if !assert.Equal(keys[i], objs[n-1-i].Key) {
					break
				}
			}
		}

		// Sort by ID descending (wrong case should still work)
		objs, _, err = client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			OrderBy: "-iD",
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				if !assert.Equal(keys[i], objs[n-1-i].Key) {
					break
				}
			}
		}

		// Sort by non-ID descending, ID is ascending by default
		objs, _, err = client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			OrderBy: "-example",
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				if !assert.Equal(keys[i], objs[i].Key) {
					break
				}
			}
		}

		// Sort by two columns
		objs, _, err = client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			OrderBy: "-example, -id",
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				if !assert.Equal(keys[i], objs[n-1-i].Key) {
					break
				}
			}
		}
	})

	t.Run("select", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 4
		keys := make([]serviceapi.ObjKey, n)
		for i := range 4 {
			var err error
			newObj := NewObject(t)
			newObj.Example = t.Name()
			keys[i], err = client.Create(ctx, newObj)
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		// Select only the example column (wrong case should still work)
		objs, _, err := client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			Select:  "exAMPle",
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				assert.Expect(
					objs[i].Key.IsZero(), true,
					objs[i].Example, t.Name(),
				)
			}
		}

		// Select only the id column
		objs, _, err = client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			Select:  "id",
		})
		if assert.NoError(err) && assert.Len(objs, n) {
			for i := range n {
				assert.Expect(
					objs[i].Key, keys[i],
					objs[i].Example, "",
				)
			}
		}
	})

	t.Run("empty_result", func(t *testing.T) {
		assert := testarossa.For(t)

		objs, totalCount, err := client.List(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(
			len(objs), 0,
			totalCount, 0,
			err, nil,
		)
	})

	t.Run("large_offset", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		for range n {
			newObj := NewObject(t)
			newObj.Example = t.Name()
			_, err := client.Create(ctx, newObj)
			assert.NoError(err)
		}

		objs, totalCount, err := client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			Offset:  1000,
		})
		assert.Expect(
			len(objs), 0,
			totalCount, n,
			err, nil,
		)
	})

	t.Run("invalid_select_column", func(t *testing.T) {
		assert := testarossa.For(t)

		_, _, err := client.List(ctx, serviceapi.Query{
			Select: "invalid-column!",
		})
		assert.Error(err)
	})

	t.Run("invalid_orderby_column", func(t *testing.T) {
		assert := testarossa.For(t)

		_, _, err := client.List(ctx, serviceapi.Query{
			OrderBy: "invalid-column!",
		})
		assert.Error(err)
	})

	t.Run("zero_limit", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		for range n {
			newObj := NewObject(t)
			newObj.Example = t.Name()
			_, err := client.Create(ctx, newObj)
			assert.NoError(err)
		}

		objs, totalCount, err := client.List(ctx, serviceapi.Query{
			Example: t.Name(),
			Limit:   0,
		})
		assert.Expect(
			len(objs), n,
			totalCount, n,
			err, nil,
		)
	})

	t.Run("text_search", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Example = "San Francisco is known as Fog City!"
		objKey1, err := client.Create(ctx, newObj)
		assert.NoError(err)

		newObj = NewObject(t)
		newObj.Example = "Los Angeles is known as: The City of Angels"
		objKey2, err := client.Create(ctx, newObj)
		assert.NoError(err)

		objs, totalCount, err := client.List(ctx, serviceapi.Query{
			Q: "Francisco",
		})
		assert.Expect(
			totalCount, 1,
			err, nil,
		)
		if assert.Len(objs, 1) {
			assert.Expect(objs[0].Key, objKey1)
		}

		objs, totalCount, err = client.List(ctx, serviceapi.Query{
			Q: "angel", // Case-insensitive, prefix match
		})
		assert.Expect(
			totalCount, 1,
			err, nil,
		)
		if assert.Len(objs, 1) {
			assert.Expect(objs[0].Key, objKey2)
		}

		objs, totalCount, err = client.List(ctx, serviceapi.Query{
			Q: "CITY", // Match both records
		})
		assert.Expect(
			totalCount, 2,
			err, nil,
		)
		if assert.Len(objs, 2) {
			assert.Expect(
				objs[0].Key, objKey1,
				objs[1].Key, objKey2,
			)
		}

		objs, totalCount, err = client.List(ctx, serviceapi.Query{
			Q: "san angeles", // Match neither record
		})
		assert.Expect(
			len(objs), 0,
			totalCount, 0,
			err, nil,
		)
	})

	t.Run("query_by_key", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			newObj := NewObject(t)
			newObj.Example = t.Name()
			keys[i], err = client.Create(ctx, newObj)
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		objs, totalCount, err := client.List(ctx, serviceapi.Query{
			Key: keys[2],
		})
		assert.Expect(
			totalCount, 1,
			err, nil,
		)
		if assert.Len(objs, 1) {
			assert.Equal(keys[2], objs[0].Key)
		}
	})

	t.Run("query_by_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			newObj := NewObject(t)
			newObj.Example = t.Name()
			keys[i], err = client.Create(ctx, newObj)
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		subset := []serviceapi.ObjKey{keys[0], keys[2], keys[4]}
		objs, totalCount, err := client.List(ctx, serviceapi.Query{
			Keys: subset,
		})
		assert.Expect(
			totalCount, 3,
			err, nil,
		)
		if assert.Len(objs, 3) {
			for i := range 3 {
				assert.Equal(subset[i], objs[i].Key)
			}
		}
	})

	t.Run("negative_limit", func(t *testing.T) {
		assert := testarossa.For(t)

		_, _, err := client.List(ctx, serviceapi.Query{
			Limit: -1,
		})
		assert.Error(err)
	})

	t.Run("negative_offset", func(t *testing.T) {
		assert := testarossa.For(t)

		_, _, err := client.List(ctx, serviceapi.Query{
			Offset: -1,
		})
		assert.Error(err)
	})
}

func TestService_Lookup(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.lookup.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("by_key", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		obj, found, err := client.Lookup(ctx, serviceapi.Query{Key: objKey})
		assert.Expect(
			obj.Key, objKey,
			found, true,
			err, nil,
		)

		deleted, err := client.Delete(ctx, objKey)
		assert.Expect(
			err, nil,
			deleted, true,
		)

		obj, found, err = client.Lookup(ctx, serviceapi.Query{Key: objKey})
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("by_example", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Example = t.Name()
		objKey, err := client.Create(ctx, newObj)
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		obj, found, err := client.Lookup(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(
			obj.Key, objKey,
			found, true,
			err, nil,
		)

		deleted, err := client.Delete(ctx, objKey)
		assert.Expect(
			err, nil,
			deleted, true,
		)

		obj, found, err = client.Lookup(ctx, serviceapi.Query{
			Key: objKey,
		})
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("not_unique", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Example = t.Name()
		objKey1, err := client.Create(ctx, newObj)
		assert.Expect(
			objKey1.IsZero(), false,
			err, nil,
		)
		newObj = NewObject(t)
		newObj.Example = t.Name()
		objKey2, err := client.Create(ctx, newObj)
		assert.Expect(
			objKey2.IsZero(), false,
			err, nil,
		)

		_, _, err = client.Lookup(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Error(err)
	})

	t.Run("nonexistent", func(t *testing.T) {
		assert := testarossa.For(t)

		obj, found, err := client.Lookup(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})
}

func TestService_MustLookup(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.mustlookup.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("exactly_one_match", func(t *testing.T) {
		assert := testarossa.For(t)

		newObj := NewObject(t)
		newObj.Example = t.Name()
		objKey1, err := client.Create(ctx, newObj)
		assert.Expect(
			objKey1.IsZero(), false,
			err, nil,
		)

		obj, err := client.MustLookup(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(
			obj.Key, objKey1,
			err, nil,
		)

		newObj = NewObject(t)
		newObj.Example = t.Name()
		objKey2, err := client.Create(ctx, newObj)
		assert.Expect(
			objKey2.IsZero(), false,
			err, nil,
		)

		_, err = client.MustLookup(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Error(err)

		deleted, err := client.Delete(ctx, objKey1)
		assert.Expect(
			err, nil,
			deleted, true,
		)

		deleted, err = client.Delete(ctx, objKey2)
		assert.Expect(
			err, nil,
			deleted, true,
		)

		_, err = client.MustLookup(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Error(err)
	})
}

func TestService_Load(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.load.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("create_load_delete", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, objKey)
		assert.Expect(
			obj != nil, true,
			obj.Key, objKey,
			found, true,
			err, nil,
		)
		deleted, err := client.Delete(ctx, objKey)
		assert.Expect(
			err, nil,
			deleted, true,
		)
		obj, found, err = client.Load(ctx, objKey)
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		obj, found, err := client.Load(ctx, serviceapi.ObjKey{})
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("nonexistent_key", func(t *testing.T) {
		assert := testarossa.For(t)

		obj, found, err := client.Load(ctx, serviceapi.ParseKey(999999))
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("concurrently", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		var wg sync.WaitGroup
		n := 10
		wg.Add(n)
		objs := make([]*serviceapi.Obj, n)
		for i := range n {
			go func() {
				defer wg.Done()
				var err error
				objs[i], _, err = client.Load(ctx, objKey)
				assert.NoError(err)
			}()
		}
		wg.Wait()

		for i := range n {
			if assert.NotNil(objs[i]) {
				assert.Equal(objKey, objs[i].Key)
			}
		}
	})

	t.Run("load_after_store", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		originalObj, found, err := client.Load(ctx, objKey)
		assert.Expect(
			originalObj != nil, true,
			found, true,
			err, nil,
		)

		originalObj.Example = "Modified"
		stored, err := client.Store(ctx, originalObj)
		assert.Expect(
			err, nil,
			stored, true,
		)

		updatedObj, found, err := client.Load(ctx, objKey)
		assert.Expect(
			updatedObj != nil, true,
			found, true,
			err, nil,
		)
		assert.Expect(updatedObj.Example, "Modified")
	})
}

func TestService_MustLoad(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.mustload.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("create_load_delete", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)
		obj, err := client.MustLoad(ctx, objKey)
		assert.Expect(
			obj != nil, true,
			obj.Key, objKey,
			err, nil,
		)
		deleted, err := client.Delete(ctx, objKey)
		assert.Expect(
			err, nil,
			deleted, true,
		)
		obj, err = client.MustLoad(ctx, objKey)
		assert.Error(err)
		assert.Nil(obj)
	})

	t.Run("load_zero_key", func(t *testing.T) {
		assert := testarossa.For(t)

		obj, err := client.MustLoad(ctx, serviceapi.ObjKey{})
		assert.Error(err)
		assert.Nil(obj)
	})

	t.Run("load_non_existent_key", func(t *testing.T) {
		assert := testarossa.For(t)

		obj, err := client.MustLoad(ctx, serviceapi.ParseKey(999999))
		assert.Error(err)
		assert.Nil(obj)
	})
}

func TestService_BulkLoad(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.bulkload.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("selective_fetch", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 10
		keys := make([]serviceapi.ObjKey, n)
		for i := 0; i < 10; i++ {
			var err error
			keys[i], err = client.Create(ctx, NewObject(t))
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		for m := range n {
			rand.Shuffle(n, func(i, j int) {
				keys[i], keys[j] = keys[j], keys[i]
			})
			loaded, err := client.BulkLoad(ctx, keys[:m])
			if assert.NoError(err) {
				expected := map[int]bool{}
				for i := range m {
					expected[keys[i].ID] = true
				}
				assert.Len(loaded, m)
				for i := range loaded {
					assert.True(expected[loaded[i].Key.ID])
				}
			}
		}
	})

	t.Run("empty_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		loaded, err := client.BulkLoad(ctx, []serviceapi.ObjKey{})
		assert.Expect(
			len(loaded), 0,
			err, nil,
		)
	})

	t.Run("duplicate_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		keys := []serviceapi.ObjKey{objKey, objKey, objKey}
		loaded, err := client.BulkLoad(ctx, keys)
		assert.Expect(
			len(loaded), 1, // Should return the object once despite duplicate keys
			err, nil,
		)
	})

	t.Run("mixed_existent_nonexistent", func(t *testing.T) {
		assert := testarossa.For(t)

		existingKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			existingKey.IsZero(), false,
			err, nil,
		)

		nonExistentKey := serviceapi.ParseKey(999999)
		keys := []serviceapi.ObjKey{existingKey, nonExistentKey}
		loaded, err := client.BulkLoad(ctx, keys)
		if assert.NoError(err) {
			assert.Len(loaded, 1)
			assert.Equal(existingKey, loaded[0].Key)
		}
	})

	t.Run("only_nonexistent_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		keys := []serviceapi.ObjKey{
			serviceapi.ParseKey(999998),
			serviceapi.ParseKey(999999),
		}
		loaded, err := client.BulkLoad(ctx, keys)
		assert.Expect(
			len(loaded), 0,
			err, nil,
		)
	})

	t.Run("nil_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		loaded, err := client.BulkLoad(ctx, nil)
		assert.Expect(
			len(loaded), 0,
			err, nil,
		)
	})
}

func TestService_BulkDelete(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.bulkdelete.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("create_and_bulk_delete", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 10
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			keys[i], err = client.Create(ctx, NewObject(t))
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		deletedKeys, err := client.BulkDelete(ctx, keys)
		sort.Slice(deletedKeys, func(i, j int) bool {
			return deletedKeys[i].ID < deletedKeys[j].ID
		})
		assert.Expect(
			deletedKeys, keys,
			err, nil,
		)

		for i := range n {
			obj, found, err := client.Load(ctx, keys[i])
			assert.Expect(
				obj, nil,
				found, false,
				err, nil,
			)
		}
	})

	t.Run("empty_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		deletedKeys, err := client.BulkDelete(ctx, []serviceapi.ObjKey{})
		assert.Expect(
			len(deletedKeys), 0,
			err, nil,
		)
	})

	t.Run("duplicate_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		keys := []serviceapi.ObjKey{objKey, objKey, objKey}
		deletedKeys, err := client.BulkDelete(ctx, keys)
		assert.Expect(
			deletedKeys, []serviceapi.ObjKey{objKey},
			err, nil,
		)

		obj, found, err := client.Load(ctx, objKey)
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("mixed_existent_nonexistent", func(t *testing.T) {
		assert := testarossa.For(t)

		existingKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			existingKey.IsZero(), false,
			err, nil,
		)

		nonExistentKey := serviceapi.ParseKey(999999)
		keys := []serviceapi.ObjKey{existingKey, nonExistentKey}
		deletedKeys, err := client.BulkDelete(ctx, keys)
		assert.Expect(
			deletedKeys, []serviceapi.ObjKey{existingKey},
			err, nil,
		)

		obj, found, err := client.Load(ctx, existingKey)
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("only_nonexistent_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		keys := []serviceapi.ObjKey{
			serviceapi.ParseKey(999998),
			serviceapi.ParseKey(999999),
		}
		deletedKeys, err := client.BulkDelete(ctx, keys)
		assert.Expect(
			len(deletedKeys), 0,
			err, nil,
		)
	})

	t.Run("nil_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		deletedKeys, err := client.BulkDelete(ctx, nil)
		assert.Expect(
			len(deletedKeys), 0,
			err, nil,
		)
	})
}

func TestService_BulkCreate(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.bulkcreate.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("create_and_load", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		objs := make([]*serviceapi.Obj, n)
		for i := range n {
			objs[i] = NewObject(t)
			objs[i].Example = t.Name() + "_" + strconv.Itoa(i)
		}
		keys, err := client.BulkCreate(ctx, objs)
		assert.Expect(
			err, nil,
		)
		if assert.Len(keys, n) {
			for i := range n {
				assert.False(keys[i].IsZero())
				obj, found, err := client.Load(ctx, keys[i])
				assert.Expect(
					obj != nil, true,
					obj.Key, keys[i],
					found, true,
					err, nil,
				)
			}
		}
	})

	t.Run("empty_input", func(t *testing.T) {
		assert := testarossa.For(t)

		keys, err := client.BulkCreate(ctx, []*serviceapi.Obj{})
		assert.Expect(
			len(keys), 0,
			err, nil,
		)
	})

	t.Run("nil_input", func(t *testing.T) {
		assert := testarossa.For(t)

		keys, err := client.BulkCreate(ctx, nil)
		assert.Expect(
			len(keys), 0,
			err, nil,
		)
	})

	t.Run("invalid_input", func(t *testing.T) {
		assert := testarossa.For(t)

		objs := []*serviceapi.Obj{
			NewObject(t),
			{Example: strings.Repeat("X", 1024)}, // Too long
			NewObject(t),
		}
		keys, err := client.BulkCreate(ctx, objs)
		assert.Error(err)
		assert.Nil(keys)
	})

	t.Run("keys_increment", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		objs := make([]*serviceapi.Obj, n)
		for i := range n {
			objs[i] = NewObject(t)
		}
		keys, err := client.BulkCreate(ctx, objs)
		assert.Expect(
			err, nil,
		)
		if assert.Len(keys, n) {
			for i := 1; i < n; i++ {
				assert.True(keys[i].ID > keys[i-1].ID)
			}
		}
	})

	t.Run("single_object", func(t *testing.T) {
		assert := testarossa.For(t)

		objs := []*serviceapi.Obj{NewObject(t)}
		keys, err := client.BulkCreate(ctx, objs)
		assert.Expect(
			err, nil,
		)
		if assert.Len(keys, 1) {
			assert.False(keys[0].IsZero())
			obj, found, err := client.Load(ctx, keys[0])
			assert.Expect(
				obj != nil, true,
				found, true,
				err, nil,
			)
		}
	})

	t.Run("nil_element_in_list", func(t *testing.T) {
		assert := testarossa.For(t)

		objs := []*serviceapi.Obj{
			NewObject(t),
			nil,
			NewObject(t),
		}
		keys, err := client.BulkCreate(ctx, objs)
		assert.Error(err)
		assert.Nil(keys)
	})
}

func TestService_BulkStore(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.bulkstore.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("create_and_bulk_store", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			newObj := NewObject(t)
			newObj.Example = "original_" + strconv.Itoa(i)
			keys[i], err = client.Create(ctx, newObj)
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		// Load all, modify, and bulk store
		objs := make([]*serviceapi.Obj, n)
		for i := range n {
			var found bool
			var err error
			objs[i], found, err = client.Load(ctx, keys[i])
			assert.Expect(
				objs[i] != nil, true,
				found, true,
				err, nil,
			)
			objs[i].Example = "modified_" + strconv.Itoa(i)
		}
		storedKeys, err := client.BulkStore(ctx, objs)
		sort.Slice(storedKeys, func(i, j int) bool {
			return storedKeys[i].ID < storedKeys[j].ID
		})
		assert.Expect(
			storedKeys, keys,
			err, nil,
		)

		// Verify modifications
		for i := range n {
			obj, found, err := client.Load(ctx, keys[i])
			assert.Expect(
				obj != nil, true,
				obj.Example, "modified_"+strconv.Itoa(i),
				found, true,
				err, nil,
			)
		}
	})

	t.Run("empty_input", func(t *testing.T) {
		assert := testarossa.For(t)

		storedKeys, err := client.BulkStore(ctx, []*serviceapi.Obj{})
		assert.Expect(
			len(storedKeys), 0,
			err, nil,
		)
	})

	t.Run("nil_input", func(t *testing.T) {
		assert := testarossa.For(t)

		storedKeys, err := client.BulkStore(ctx, nil)
		assert.Expect(
			len(storedKeys), 0,
			err, nil,
		)
	})

	t.Run("invalid_input", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			found, true,
			err, nil,
		)

		objs := []*serviceapi.Obj{
			obj,
			{Key: key, Example: strings.Repeat("X", 1024)}, // Too long
		}
		storedKeys, err := client.BulkStore(ctx, objs)
		assert.Error(err)
		assert.Nil(storedKeys)
	})

	t.Run("nil_object_in_list", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			found, true,
			err, nil,
		)

		objs := []*serviceapi.Obj{obj, nil}
		storedKeys, err := client.BulkStore(ctx, objs)
		assert.Error(err)
		assert.Nil(storedKeys)
	})

	t.Run("zero_key_in_list", func(t *testing.T) {
		assert := testarossa.For(t)

		objs := []*serviceapi.Obj{
			{Example: "test"},
		}
		storedKeys, err := client.BulkStore(ctx, objs)
		assert.Error(err)
		assert.Nil(storedKeys)
	})

	t.Run("nonexistent_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		obj1 := NewObject(t)
		obj1.Key = serviceapi.ParseKey(999998)
		obj2 := NewObject(t)
		obj2.Key = serviceapi.ParseKey(999999)
		storedKeys, err := client.BulkStore(ctx, []*serviceapi.Obj{obj1, obj2})
		assert.Expect(
			len(storedKeys), 0,
			err, nil,
		)
	})

	t.Run("single_object", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			found, true,
			err, nil,
		)
		obj.Example = "bulk_stored"
		storedKeys, err := client.BulkStore(ctx, []*serviceapi.Obj{obj})
		assert.Expect(
			storedKeys, []serviceapi.ObjKey{key},
			err, nil,
		)
		obj, found, err = client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			obj.Example, "bulk_stored",
			found, true,
			err, nil,
		)
	})
}

func TestService_BulkRevise(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.bulkrevise.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("all_matching_revisions", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			newObj := NewObject(t)
			newObj.Example = "original_" + strconv.Itoa(i)
			keys[i], err = client.Create(ctx, newObj)
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		// Load all, modify, and bulk revise
		objs := make([]*serviceapi.Obj, n)
		for i := range n {
			var found bool
			var err error
			objs[i], found, err = client.Load(ctx, keys[i])
			assert.Expect(
				objs[i] != nil, true,
				found, true,
				err, nil,
			)
			objs[i].Example = "revised_" + strconv.Itoa(i)
		}
		revisedKeys, err := client.BulkRevise(ctx, objs)
		sort.Slice(revisedKeys, func(i, j int) bool {
			return revisedKeys[i].ID < revisedKeys[j].ID
		})
		assert.Expect(
			revisedKeys, keys,
			err, nil,
		)

		// Verify modifications
		for i := range n {
			obj, found, err := client.Load(ctx, keys[i])
			assert.Expect(
				obj != nil, true,
				obj.Example, "revised_"+strconv.Itoa(i),
				found, true,
				err, nil,
			)
		}
	})

	t.Run("some_stale_revisions", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 4
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			keys[i], err = client.Create(ctx, NewObject(t))
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		// Load all
		objs := make([]*serviceapi.Obj, n)
		for i := range n {
			var found bool
			var err error
			objs[i], found, err = client.Load(ctx, keys[i])
			assert.Expect(
				objs[i] != nil, true,
				found, true,
				err, nil,
			)
		}

		// Store half to bump their revisions
		for i := range n / 2 {
			stored, err := client.Store(ctx, objs[i])
			assert.Expect(
				err, nil,
				stored, true,
			)
		}
		unbumpedKeys := []serviceapi.ObjKey{}
		for i := n / 2; i < n; i++ {
			unbumpedKeys = append(unbumpedKeys, objs[i].Key)
		}

		// BulkRevise all with original revisions - only the un-bumped half should succeed
		for i := range n {
			objs[i].Example = "revised"
		}
		revisedKeys, err := client.BulkRevise(ctx, objs)
		assert.Expect(
			revisedKeys, unbumpedKeys,
			err, nil,
		)
	})

	t.Run("empty_input", func(t *testing.T) {
		assert := testarossa.For(t)

		revisedKeys, err := client.BulkRevise(ctx, []*serviceapi.Obj{})
		assert.Expect(
			len(revisedKeys), 0,
			err, nil,
		)
	})

	t.Run("nil_input", func(t *testing.T) {
		assert := testarossa.For(t)

		revisedKeys, err := client.BulkRevise(ctx, nil)
		assert.Expect(
			len(revisedKeys), 0,
			err, nil,
		)
	})

	t.Run("nil_object_in_list", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			found, true,
			err, nil,
		)

		objs := []*serviceapi.Obj{obj, nil}
		revisedKeys, err := client.BulkRevise(ctx, objs)
		assert.Error(err)
		assert.Len(revisedKeys, 0)
	})

	t.Run("nonexistent_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		obj1 := NewObject(t)
		obj1.Key = serviceapi.ParseKey(999998)
		obj2 := NewObject(t)
		obj2.Key = serviceapi.ParseKey(999999)
		revisedKeys, err := client.BulkRevise(ctx, []*serviceapi.Obj{obj1, obj2})
		assert.Expect(
			len(revisedKeys), 0,
			err, nil,
		)
	})

	t.Run("single_object", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			found, true,
			err, nil,
		)
		obj.Example = "bulk_revised"
		revisedKeys, err := client.BulkRevise(ctx, []*serviceapi.Obj{obj})
		assert.Expect(
			revisedKeys, []serviceapi.ObjKey{obj.Key},
			err, nil,
		)
		obj, found, err = client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			obj.Example, "bulk_revised",
			found, true,
			err, nil,
		)
	})

	t.Run("invalid_input", func(t *testing.T) {
		assert := testarossa.For(t)

		key, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			key.IsZero(), false,
			err, nil,
		)
		obj, found, err := client.Load(ctx, key)
		assert.Expect(
			obj != nil, true,
			found, true,
			err, nil,
		)

		objs := []*serviceapi.Obj{
			obj,
			{Key: key, Revision: 1, Example: strings.Repeat("X", 1024)}, // Too long
		}
		revisedKeys, err := client.BulkRevise(ctx, objs)
		assert.Error(err)
		assert.Len(revisedKeys, 0)
	})

	t.Run("zero_key_in_list", func(t *testing.T) {
		assert := testarossa.For(t)

		objs := []*serviceapi.Obj{
			{Revision: 1, Example: "test"},
		}
		revisedKeys, err := client.BulkRevise(ctx, objs)
		assert.Error(err)
		assert.Len(revisedKeys, 0)
	})
}

func TestService_Purge(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.purge.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("purge_by_example", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 10
		for range n {
			newObj := NewObject(t)
			newObj.Example = t.Name()
			_, err := client.Create(ctx, newObj)
			assert.NoError(err)
		}

		// Create some objects with a different example to ensure they survive
		for range 3 {
			newObj := NewObject(t)
			newObj.Example = t.Name() + "_other"
			_, err := client.Create(ctx, newObj)
			assert.NoError(err)
		}

		deletedKeys, err := client.Purge(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(
			len(deletedKeys), n,
			err, nil,
		)

		count, err := client.Count(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(
			count, 0,
			err, nil,
		)

		count, err = client.Count(ctx, serviceapi.Query{
			Example: t.Name() + "_other",
		})
		assert.Expect(
			count, 3,
			err, nil,
		)
	})

	t.Run("purge_by_key", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		deletedKeys, err := client.Purge(ctx, serviceapi.Query{
			Key: objKey,
		})
		assert.Expect(
			deletedKeys, []serviceapi.ObjKey{objKey},
			err, nil,
		)

		obj, found, err := client.Load(ctx, objKey)
		assert.Expect(
			obj, nil,
			found, false,
			err, nil,
		)
	})

	t.Run("purge_by_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			keys[i], err = client.Create(ctx, NewObject(t))
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		deletedKeys, err := client.Purge(ctx, serviceapi.Query{
			Keys: keys,
		})
		sort.Slice(deletedKeys, func(i, j int) bool {
			return deletedKeys[i].ID < deletedKeys[j].ID
		})
		assert.Expect(
			deletedKeys, keys,
			err, nil,
		)

		objs, err := client.BulkLoad(ctx, keys)
		assert.Expect(
			len(objs), 0,
			err, nil,
		)
	})

	t.Run("purge_no_match", func(t *testing.T) {
		assert := testarossa.For(t)

		deletedKeys, err := client.Purge(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(
			len(deletedKeys), 0,
			err, nil,
		)
	})

	t.Run("purge_empty_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		deletedKeys, err := client.Purge(ctx, serviceapi.Query{
			Keys: []serviceapi.ObjKey{},
		})
		assert.Expect(
			len(deletedKeys), 0,
			err, nil,
		)
	})
}

func TestService_Count(t *testing.T) {
	t.Skip() // Don't test inside the bundle
	t.Parallel()
	ctx := t.Context()
	_ = ctx

	// Initialize the microservice under test
	svc := NewService()
	// svc.SetSQLDataSourceName(dsn)

	// Initialize the testers
	tester := connector.New("objs.count.tester")
	client := serviceapi.NewClient(tester)
	_ = client

	// Run the testing app
	app := application.New()
	app.Add(
		// HINT: Add microservices or mocks required for this test
		svc,
		tester,
	)
	app.RunInTest(t)

	t.Run("count", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 10
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			newObj := NewObject(t)
			newObj.Example = t.Name()
			keys[i], err = client.Create(ctx, newObj)
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		count, err := client.Count(ctx, serviceapi.Query{
			Limit:   0,
			Example: t.Name(),
		})
		assert.Expect(count, n, err, nil)

		count, err = client.Count(ctx, serviceapi.Query{
			Limit:   n / 2,
			Example: t.Name(),
		})
		assert.Expect(count, n, err, nil)

		count, err = client.Count(ctx, serviceapi.Query{
			Limit:   n / 2,
			Example: t.Name(),
		})
		assert.Expect(count, n, err, nil)

		for i := 0; i < n/2; i++ {
			deleted, err := client.Delete(ctx, keys[i])
			assert.Expect(
				err, nil,
				deleted, true,
			)
		}

		count, err = client.Count(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(count, n/2, err, nil)
	})

	t.Run("empty", func(t *testing.T) {
		assert := testarossa.For(t)

		count, err := client.Count(ctx, serviceapi.Query{
			Example: t.Name(),
		})
		assert.Expect(
			count, 0,
			err, nil,
		)
	})

	t.Run("ignore_offset", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 10
		for range n {
			newObj := NewObject(t)
			newObj.Example = t.Name()
			_, err := client.Create(ctx, newObj)
			assert.NoError(err)
		}

		count, err := client.Count(ctx, serviceapi.Query{
			Example: t.Name(),
			Offset:  5,
		})
		assert.Expect(
			count, n,
			err, nil,
		)
	})

	t.Run("count_by_key", func(t *testing.T) {
		assert := testarossa.For(t)

		objKey, err := client.Create(ctx, NewObject(t))
		assert.Expect(
			objKey.IsZero(), false,
			err, nil,
		)

		count, err := client.Count(ctx, serviceapi.Query{
			Key: objKey,
		})
		assert.Expect(
			count, 1,
			err, nil,
		)

		deleted, err := client.Delete(ctx, objKey)
		assert.Expect(
			err, nil,
			deleted, true,
		)

		count, err = client.Count(ctx, serviceapi.Query{
			Key: objKey,
		})
		assert.Expect(
			count, 0,
			err, nil,
		)
	})

	t.Run("count_by_keys", func(t *testing.T) {
		assert := testarossa.For(t)

		n := 5
		keys := make([]serviceapi.ObjKey, n)
		for i := range n {
			var err error
			keys[i], err = client.Create(ctx, NewObject(t))
			assert.Expect(
				keys[i].IsZero(), false,
				err, nil,
			)
		}

		count, err := client.Count(ctx, serviceapi.Query{
			Keys: keys[:3],
		})
		assert.Expect(
			count, 3,
			err, nil,
		)
	})
}

// NewObject creates a new valid object for a test.
// This function must be safe for concurrent use.
func NewObject(t *testing.T) *serviceapi.Obj {
	examples := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	return &serviceapi.Obj{
		// HINT: Initialize object fields here
		Example: examples[rand.Intn(len(examples))],
	}
}
