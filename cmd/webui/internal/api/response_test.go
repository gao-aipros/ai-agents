package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsHTMX(t *testing.T) {
	t.Run("no header", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/", nil)
		if IsHTMX(r) {
			t.Error("expected false without HX-Request header")
		}
	})

	t.Run("header set to true", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("HX-Request", "true")
		if !IsHTMX(r) {
			t.Error("expected true with HX-Request: true")
		}
	})

	t.Run("header set to false", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("HX-Request", "false")
		if IsHTMX(r) {
			t.Error("expected false with HX-Request: false")
		}
	})
}

func TestJSON(t *testing.T) {
	w := httptest.NewRecorder()
	JSON(w, http.StatusOK, map[string]string{"key": "value"})

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	if body := w.Body.String(); body != "{\"key\":\"value\"}\n" {
		t.Errorf("body = %q, want %q", body, "{\"key\":\"value\"}\n")
	}
}

func TestError(t *testing.T) {
	w := httptest.NewRecorder()
	Error(w, http.StatusBadRequest, "something went wrong")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	want := `{"error":"something went wrong"}` + "\n"
	if body := w.Body.String(); body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

func TestRespond(t *testing.T) {
	t.Run("without HTMX returns JSON", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		Respond(w, r, http.StatusOK, map[string]int{"count": 42})

		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want %q", ct, "application/json")
		}
	})

	t.Run("with HTMX also returns JSON for now", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("HX-Request", "true")
		Respond(w, r, http.StatusOK, map[string]int{"count": 42})

		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want %q", ct, "application/json")
		}
	})
}
