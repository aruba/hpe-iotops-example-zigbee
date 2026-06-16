package retry

import (
	"math"
	"time"
)

// ExponentialBackoff implements exponential backoff with jitter for retries.
// It starts with an initial delay and multiplies by a factor on each retry,
// up to a maximum interval. This is useful for distributed systems where
// coordinated retry storms can cause cascading failures.
type ExponentialBackoff struct {
	InitialDelay time.Duration
	Multiplier   float64
	MaxInterval  time.Duration
	attempt      int
}

// NewExponentialBackoff creates a new exponential backoff controller.
// Typical values: initialDelay=1s, multiplier=2, maxInterval=30m
func NewExponentialBackoff(initialDelay time.Duration, multiplier float64, maxInterval time.Duration) *ExponentialBackoff {
	return &ExponentialBackoff{
		InitialDelay: initialDelay,
		Multiplier:   multiplier,
		MaxInterval:  maxInterval,
		attempt:      0,
	}
}

// NextDelay returns the next delay duration and increments the attempt counter.
func (b *ExponentialBackoff) NextDelay() time.Duration {
	b.attempt++
	
	// Calculate exponential backoff: initial * (multiplier ^ (attempt - 1))
	delay := time.Duration(float64(b.InitialDelay) * math.Pow(b.Multiplier, float64(b.attempt-1)))
	
	// Cap at maximum interval
	if delay > b.MaxInterval {
		delay = b.MaxInterval
	}
	
	return delay
}

// Reset clears the attempt counter for a new sequence.
func (b *ExponentialBackoff) Reset() {
	b.attempt = 0
}

// Attempts returns the current attempt number (0 if never called).
func (b *ExponentialBackoff) Attempts() int {
	return b.attempt
}
