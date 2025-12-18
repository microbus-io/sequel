package serviceapi

import (
	"context"
	"strings"
	"time"

	"github.com/microbus-io/errors"
)

// Obj
type Obj struct {
	Key       ObjKey    `json:"key,omitzero"`
	Revision  int       `json:"revision,omitzero"`
	CreatedAt time.Time `json:"createdAt,omitzero"`
	UpdatedAt time.Time `json:"updatedAt,omitzero"`

	// HINT: Define the fields of the object here
	Example string `json:"example,omitzero" jsonschema:"-"` // Do not remove the example
}

// Validate validates the object before storing it.
func (obj *Obj) Validate(ctx context.Context) error {
	if obj == nil {
		return errors.New("nil object")
	}
	// HINT: Validate the fields of the object here as required
	obj.Example = strings.TrimSpace(obj.Example) // Do not remove the example
	if len([]rune(obj.Example)) > 256 {
		return errors.New("length of Example must not exceed 256 characters")
	}
	return nil
}
