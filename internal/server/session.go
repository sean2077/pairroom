package server

import (
	"context"
	"crypto/subtle"
	"net/http"

	"github.com/sean2077/pairroom/internal/websession"
)

const (
	browserSessionCookie = "pairroom_session"
	csrfHeaderName       = websession.CSRFHeaderName
)

type authMode uint8

const (
	authNone authMode = iota
	authBearer
	authBrowserSession
)

type authContextKey struct{}

type requestAuth struct {
	Mode    authMode
	Session websession.Session
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func authFromContext(ctx context.Context) requestAuth {
	value, _ := ctx.Value(authContextKey{}).(requestAuth)
	return value
}

func withAuth(r *http.Request, value requestAuth) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authContextKey{}, value))
}
