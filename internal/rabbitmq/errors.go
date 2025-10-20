package rabbitmq

import (
    "errors"
)

// ErrConnectionNotInitilized is returned when an operation requires an active
// connection but the ConnectionManager has not yet established one.
var ErrConnectionNotInitilized = errors.New("connection not initialized")

// ErrConnectionClosed is returned when an operation cannot proceed because the
// underlying AMQP connection has already been closed.
var ErrConnectionClosed = errors.New("connection closed")

// ErrNacked indicates that the broker negatively acknowledged a published
// message (i.e., publish confirm received with nack). Callers may treat this
// as a transient error and retry according to their policy.
var ErrNacked = errors.New("publish not confirmed (nack)")

// ErrConfirmTimeout indicates that no publisher confirm was received within the
// expected time window. This often points to a network partition or a stalled
// channel. Retrying after reinitializing the channel is typically appropriate.
var ErrConfirmTimeout = errors.New("publish not confirmed (timeout)")

var ErrChannelReinitBackoffExceed = errors.New("channel reinit backoff exceeded")

var ErrRepublishBackoffExceed = errors.New("republish backoff exceeded")
