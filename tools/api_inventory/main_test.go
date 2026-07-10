package main

import (
	"bytes"
	"testing"
)

func TestNormalizeLineEndings(t *testing.T) {
	got := normalizeLineEndings([]byte("one\r\ntwo\n"))
	if !bytes.Equal(got, []byte("one\ntwo\n")) {
		t.Fatalf("normalized = %q", got)
	}
}
