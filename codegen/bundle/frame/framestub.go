package frame

import "context"

// Frame is a stub.
type Frame struct{}

func Of(ctx context.Context) Frame {
	return Frame{}
}

func (f Frame) Tenant() (int, error) {
	return 0, nil
}
