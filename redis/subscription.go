package redis

import (
	"log"
	"sync"
)

type subType uint8

const (
	subSubscribe subType = iota
	subUnsubscribe
	subPsubscribe
	subPunsubscribe
)

// Subscription is a structure for holding a Redis subscription for multiple channels.
type Subscription struct {
	conn      *connection
	msgHdlr   func(msg *Message)
	lock      sync.Mutex
	listening bool
}

// newSubscription returns a new Subscription or an error.
func newSubscription(config *Configuration, msgHdlr func(msg *Message)) (*Subscription, *Error) {
	var err *Error

	s := &Subscription{
		msgHdlr: msgHdlr,
	}

	// Connection handling
	s.conn, err = newConnection(config)
	if err != nil {
		return nil, err
	}

	s.conn.noRTimeout = true // disable read timeout during pubsub mode
	return s, nil
}

// listen starts the listener goroutine, if it's not running already.
func (s *Subscription) listen() {
	s.lock.Lock()
	if !s.listening {
		s.listening = true
		go s.listener()
	}
}

// Subscribe subscribes to given channels or returns an error.
func (s *Subscription) Subscribe(channels ...string) (err *Error) {
	s.listen()
	err = s.conn.subscription(subSubscribe, channels)
	s.lock.Unlock()
	return err
}

// Unsubscribe unsubscribes from given channels or returns an error.
func (s *Subscription) Unsubscribe(channels ...string) (err *Error) {
	s.listen()
	err = s.conn.subscription(subUnsubscribe, channels)
	s.lock.Unlock()
	return err
}

// Psubscribe subscribes to given patterns or returns an error.
func (s *Subscription) Psubscribe(patterns ...string) (err *Error) {
	s.listen()
	err = s.conn.subscription(subPsubscribe, patterns)
	s.lock.Unlock()
	return err
}

// Punsubscribe unsubscribes from given patterns or returns an error.
func (s *Subscription) Punsubscribe(patterns ...string) (err *Error) {
	s.listen()
	err = s.conn.subscription(subPunsubscribe, patterns)
	s.lock.Unlock()
	return err
}

// Close closes the subscription.
func (s *Subscription) Close() {
	// just sack the connection, listener will close down eventually.
	s.conn.close()
}

// parseResponse parses the given pubsub message data and returns it as a message.
func (s *Subscription) parseResponse(rd *readData) *message {
	r := s.conn.receiveReply(rd)
	var r0, r1 *Reply
	m := &message{}

	if r.Type == ReplyError {
		goto Errmsg
	}

	if r.Type != ReplyMulti || r.Len() < 3 {
		goto Errmsg
	}

	r0 = r.At(0)
	if r0.Type != ReplyString {
		goto Errmsg
	}

	// first argument is the message type
	switch r0.Str() {
	case "subscribe":
		m.type_ = messageSubscribe
	case "unsubscribe":
		m.type_ = messageUnsubscribe
	case "psubscribe":
		m.type_ = messagePsubscribe
	case "punsubscribe":
		m.type_ = messagePunsubscribe
	case "message":
		m.type_ = messageMessage
	case "pmessage":
		m.type_ = messagePmessage
	default:
		goto Errmsg
	}

	// second argument
	r1 = r.At(1)
	if r1.Type != ReplyString {
		goto Errmsg
	}

	switch {
	case m.type_ == messageSubscribe || m.type_ == messageUnsubscribe:
		m.channel = r1.Str()

		// number of subscriptions
		r2 := r.At(2)
		if r2.Type != ReplyInteger {
			goto Errmsg
		}

		m.subscriptions = r2.Int()
	case m.type_ == messagePsubscribe || m.type_ == messagePunsubscribe:
		m.pattern = r1.Str()

		// number of subscriptions
		r2 := r.At(2)
		if r2.Type != ReplyInteger {
			goto Errmsg
		}

		m.subscriptions = r2.Int()
	case m.type_ == messageMessage:
		m.channel = r1.Str()

		// payload
		r2 := r.At(2)
		if r2.Type != ReplyString {
			goto Errmsg
		}

		m.payload = r2.Str()
	case m.type_ == messagePmessage:
		m.pattern = r1.Str()

		// name of the originating channel
		r2 := r.At(2)
		if r2.Type != ReplyString {
			goto Errmsg
		}

		m.channel = r2.Str()

		// payload
		r3 := r.At(3)
		if r3.Type != ReplyString {
			goto Errmsg
		}

		m.payload = r3.Str()
	default:
		goto Errmsg
	}

	return m

Errmsg:
	// Error/Invalid message reply
	// we shouldn't generally get these, unless there's a bug.
	log.Println("received errorneous/invalid reply while in pubsub mode! ignoring...")
	return nil
}

// listener is a goroutine for reading and handling pubsub messages.
func (s *Subscription) listener() {
	var m *message

	// read until connection is closed or
	// when subscription count reaches zero
	for {
		rd := s.conn.read()
		s.lock.Lock()
		if rd.error != nil && rd.error.Test(ErrorConnection) {
			// connection closed
			s.listening = false
			s.lock.Unlock()
			return
		}

		m = s.parseResponse(rd)
		if (m.type_ == messageSubscribe ||
			m.type_ == messageUnsubscribe ||
			m.type_ == messagePsubscribe ||
			m.type_ == messagePunsubscribe) && m.subscriptions == 0 {
			s.listening = false
			s.lock.Unlock()
			return
		}
		s.lock.Unlock()

		if m.type_ == messageMessage || m.type_ == messagePmessage {
			go s.msgHdlr(newMessage(m))
		}

	}
}