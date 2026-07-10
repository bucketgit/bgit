package web

import (
	"strings"
	"testing"
)

func TestEventHubBroadcastsAndUnsubscribes(t *testing.T) {
	hub := NewEventHub()
	channel := hub.Subscribe()
	hub.BroadcastJSON("state", map[string]bool{"dirty": true})
	if event := <-channel; !strings.Contains(event, "event: state") || !strings.Contains(event, `"dirty":true`) {
		t.Fatalf("event=%q", event)
	}
	hub.Unsubscribe(channel)
	if _, ok := <-channel; ok {
		t.Fatal("unsubscribed channel remains open")
	}
}
