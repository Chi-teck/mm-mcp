package mattermost

import (
	"errors"
	"fmt"

	"github.com/mattermost/mattermost/server/public/model"
)

// NoServerMessage stands in for an empty server error message.
const NoServerMessage = "the server sent no message"

// APIError is a failed Mattermost API call, restated as one readable sentence.
type APIError struct {
	Status  int    // HTTP status code
	Path    string // request path without scheme and host, e.g. "/api/v4/users/me"
	Message string // server message; empty when the server sent none
}

// Error renders `mattermost API <status> <path>: <message>`.
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = NoServerMessage
	}
	return fmt.Sprintf("mattermost API %d %s: %s", e.Status, e.Path, msg)
}

// WrapErr turns a *model.AppError returned by a Client4 call on path into an
// *APIError. Other errors (transport, cancellation) and nil pass through
// unchanged. path must not include the scheme and host.
func WrapErr(path string, err error) error {
	var appErr *model.AppError
	if !errors.As(err, &appErr) {
		return err
	}
	return &APIError{Status: appErr.StatusCode, Path: path, Message: appErr.Message}
}
