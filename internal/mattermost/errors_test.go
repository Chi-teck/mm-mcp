package mattermost

import (
	"errors"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
)

func TestWrapErr(t *testing.T) {
	if WrapErr("/p", nil) != nil {
		t.Fatal("WrapErr(nil) != nil")
	}
	plain := errors.New("dial tcp: refused")
	if got := WrapErr("/p", plain); got != plain { //nolint:errorlint // identity check
		t.Fatalf("plain error changed: %v", got)
	}
	appErr := &model.AppError{Id: "x", Message: "Unable to find.", StatusCode: 404}
	got := WrapErr("/api/v4/users/me", appErr)
	apiErr, ok := errors.AsType[*APIError](got)
	if !ok {
		t.Fatalf("not *APIError: %T", got)
	}
	if *apiErr != (APIError{Status: 404, Path: "/api/v4/users/me", Message: "Unable to find."}) {
		t.Fatalf("got %+v", *apiErr)
	}
}

func TestAPIErrorError(t *testing.T) {
	tests := []struct {
		err  APIError
		want string
	}{
		{APIError{403, "/api/v4/posts", "No permission."}, "mattermost API 403 /api/v4/posts: No permission."},
		{APIError{502, "/api/v4/users/me", ""}, "mattermost API 502 /api/v4/users/me: the server sent no message"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}
