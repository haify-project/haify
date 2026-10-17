package main

import "testing"

func TestWithPage(t *testing.T) {
	if got := withPage("Resources", "why?"); got != "(Asked from the web UI's Resources page.)\n\nwhy?" {
		t.Fatalf("got %q", got)
	}
	for _, page := range []string{"", "  ", "a\nb", string(make([]byte, 200))} {
		if got := withPage(page, "why?"); got != "why?" {
			t.Fatalf("page %q: got %q", page, got)
		}
	}
}
