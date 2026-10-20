package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, strings.NewReader(""), &out, &errb); code != 2 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestRunUnknownAction(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"tm", "frobnicate", "a"}, strings.NewReader(""), &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "haify: tm frobnicate: unsupported action") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("nothing is printed on stdout on failure: %q", out.String())
	}
}
