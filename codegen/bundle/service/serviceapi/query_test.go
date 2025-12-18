package serviceapi

import (
	"strings"
	"testing"

	"github.com/microbus-io/testarossa"
)

func TestQuery_ValidateObject(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	assert := testarossa.For(t)

	// Prepare a valid object
	validObject := Obj{
		// HINT: Initialize the object's fields with valid values
		Example: "Valid value",
	}
	err := validObject.Validate(ctx)
	assert.NoError(err)

	// HINT: Check validation of individual object fields
	t.Run("example_too_long", func(t *testing.T) {
		assert := testarossa.For(t)
		invalidObject := validObject
		invalidObject.Example = strings.Repeat("X", 1024) // Too long
		assert.Error(invalidObject.Validate(ctx))
	})
}
