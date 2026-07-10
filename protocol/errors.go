package protocol

import (
	"errors"
	"fmt"
)

var (
	ErrConflict         = errors.New("bucketgit conflict")
	ErrUnauthorized     = errors.New("bucketgit unauthorized")
	ErrUnsupported      = errors.New("bucketgit unsupported")
	ErrSignatureExpired = errors.New("bucketgit signature timestamp expired")
	ErrReplay           = errors.New("bucketgit signature replay")
)

type BrokerError struct {
	Endpoint string
	Status   int
	Code     string
	Message  string
	Kind     error
}

func (e *BrokerError) Error() string {
	if e == nil {
		return "<nil>"
	}
	prefix := "broker"
	if e.Endpoint != "" {
		prefix += " " + e.Endpoint
	}
	if e.Message == "" {
		return prefix
	}
	return fmt.Sprintf("%s: %s", prefix, e.Message)
}

func (e *BrokerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}
