package web

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type EventHub struct {
	mu      sync.Mutex
	clients map[chan string]struct{}
}

func NewEventHub() *EventHub {
	return &EventHub{clients: map[chan string]struct{}{}}
}

func (h *EventHub) Subscribe() chan string {
	channel := make(chan string, 8)
	h.mu.Lock()
	h.clients[channel] = struct{}{}
	h.mu.Unlock()
	return channel
}

func (h *EventHub) Unsubscribe(channel chan string) {
	h.mu.Lock()
	delete(h.clients, channel)
	close(channel)
	h.mu.Unlock()
}

func (h *EventHub) Broadcast(name string) {
	h.send(fmt.Sprintf("event: %s\ndata: {\"time\":%d}\n\n", name, time.Now().UnixMilli()))
}

func (h *EventHub) BroadcastJSON(name string, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		return
	}
	h.send(fmt.Sprintf("event: %s\ndata: %s\n\n", name, data))
}

func (h *EventHub) send(payload string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for channel := range h.clients {
		select {
		case channel <- payload:
		default:
		}
	}
}
