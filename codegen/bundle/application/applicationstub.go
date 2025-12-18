package application

import (
	"testing"
)

// Application is a stub.
type Application struct{}

func (a *Application) Add(args ...any) {
}

func (a *Application) RunInTest(t *testing.T) {
}

func New() *Application {
	return nil
}

func NewTesting() *Application {
	return nil
}
