package serviceapi

import (
	"testing"
)

func BenchmarkEncrypt(b *testing.B) {
	for i := range b.N {
		keyEncrypt(i + 1)
	}
}

func BenchmarkDecrypt(b *testing.B) {
	s := keyEncrypt(1234)
	for b.Loop() {
		keyDecrypt(s)
	}
}
