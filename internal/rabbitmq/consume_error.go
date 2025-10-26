package rabbitmq

// ConsumeError wraps an error returned by a consumer callback and carries
// handling hints for the consumer loop (e.g., whether to republish the
// message).
type ConsumeError struct {
	Err       error
	Republish bool
}

// Error implements the error interface.
func (e *ConsumeError) Error() string {
	return e.Err.Error()
}

// ConsumeErrorOption configures a ConsumeError via functional options.
// Use with NewConsumeError.
type ConsumeErrorOption func(*ConsumeError)

// WithRepublish marks the error as requiring the message to be republished.
func WithRepublish() ConsumeErrorOption {
	return func(e *ConsumeError) {
		e.Republish = true
	}
}

// NewConsumeError creates a new ConsumeError from the given base error and
// applies any provided options.
func NewConsumeError(err error, opts ...ConsumeErrorOption) *ConsumeError {
	ce := &ConsumeError{Err: err}
	for _, opt := range opts {
		opt(ce)
	}
	return ce
}
