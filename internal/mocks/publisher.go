package mocks

import (
	"context"
	"sync"
)

// Message is one message the fake publisher acknowledged.
type Message struct {
	Subject string
	MsgID   string
	Headers map[string]string
	Data    []byte
}

// Publisher is a fake JetStream publisher. It acknowledges every message
// and keeps duplicate Nats-Msg-Id values (no duplicate window)
// so tests can see re-publication.
type Publisher struct {
	mu       sync.Mutex
	messages []Message
	// OnPublish, when set, runs after a message was acknowledged.
	OnPublish func(Message)
}

// Publish implements outbox.Publisher.
func (p *Publisher) Publish(ctx context.Context, subject, msgID string, headers map[string]string, data []byte) error {
	p.mu.Lock()
	h := map[string]string{}
	for k, v := range headers {
		h[k] = v
	}
	msg := Message{Subject: subject, MsgID: msgID, Headers: h, Data: append([]byte(nil), data...)}
	p.messages = append(p.messages, msg)
	hook := p.OnPublish
	p.mu.Unlock()
	if hook != nil {
		hook(msg)
	}
	return nil
}

// Messages returns the acknowledged messages in publication order.
func (p *Publisher) Messages() []Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Message(nil), p.messages...)
}
