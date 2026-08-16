package websession

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStoreCreatesRefreshesAndExpiresSession(t *testing.T) {
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	store, err := New("pairroom_test_session")
	if err != nil {
		t.Fatal(err)
	}
	store.ttl = 10 * time.Minute
	store.now = func() time.Time { return now }

	created := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/session", nil)
	session, err := store.Create(created, request)
	if err != nil {
		t.Fatal(err)
	}
	cookies := created.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected browser session cookie: %#v", cookies)
	}
	if !session.ValidCSRF(session.CSRFToken) || session.ValidCSRF("different") {
		t.Fatal("CSRF token comparison failed")
	}

	now = now.Add(5 * time.Minute)
	refreshed := httptest.NewRecorder()
	refreshRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/session", nil)
	refreshRequest.AddCookie(cookies[0])
	value, ok := store.Get(refreshed, refreshRequest)
	if !ok || !value.ExpiresAt.Equal(now.Add(10*time.Minute)) || len(refreshed.Result().Cookies()) != 1 {
		t.Fatalf("session was not refreshed: ok=%v session=%#v", ok, value)
	}

	now = now.Add(11 * time.Minute)
	expired := httptest.NewRecorder()
	expiredRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/session", nil)
	expiredRequest.AddCookie(cookies[0])
	if _, ok := store.Get(expired, expiredRequest); ok {
		t.Fatal("expired session remained valid")
	}
}

func TestStoreRejectsInvalidCookieNameAndClearsSession(t *testing.T) {
	if (Session{}).ValidCSRF("") {
		t.Fatal("empty session accepted empty CSRF token")
	}
	if _, err := New("invalid cookie"); err == nil {
		t.Fatal("invalid cookie name was accepted")
	}
	store, err := New("pairroom_test_session")
	if err != nil {
		t.Fatal(err)
	}
	created := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/session", nil)
	if _, err := store.Create(created, request); err != nil {
		t.Fatal(err)
	}
	cookie := created.Result().Cookies()[0]

	deleted := httptest.NewRecorder()
	deleteRequest := httptest.NewRequest(http.MethodDelete, "http://127.0.0.1/api/v1/session", nil)
	deleteRequest.AddCookie(cookie)
	store.Delete(deleted, deleteRequest)
	if cleared := deleted.Result().Cookies(); len(cleared) != 1 || cleared[0].MaxAge != -1 {
		t.Fatalf("session cookie was not cleared: %#v", cleared)
	}
	after := httptest.NewRecorder()
	afterRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/session", nil)
	afterRequest.AddCookie(cookie)
	if _, ok := store.Get(after, afterRequest); ok {
		t.Fatal("deleted session remained valid")
	}
}
