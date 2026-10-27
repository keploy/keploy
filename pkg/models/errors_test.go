package models

import (
	"errors"
	"fmt"
	"testing"
)

func TestUnreadCause(t *testing.T) {
	if got := UnreadCause(fmt.Errorf("%w: keploy's error drain stopped", ErrUnmatchedCallsUnread)); got != "keploy's error drain stopped" {
		t.Fatalf("got %q, want the cause alone", got)
	}
	if got := UnreadCause(errors.New("no sentinel")); got != "no sentinel" {
		t.Fatalf("got %q, want all of it", got)
	}
	if got := UnreadCause(nil); got != "" {
		t.Fatalf("got %q for no error", got)
	}
}
