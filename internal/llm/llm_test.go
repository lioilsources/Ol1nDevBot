package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" || req.Model != "m" || len(req.Messages) != 2 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"bad request"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"  ahoj  "}}]}`))
	}))
	defer srv.Close()

	out, err := New(srv.URL+"/v1/", "k", "m").Complete(context.Background(), "sys", "user")
	if err != nil || out != "ahoj" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := New(srv.URL+"/v1", "wrong", "m").Complete(context.Background(), "s", "u"); err == nil || err.Error() != "llm status 400: bad request" {
		t.Fatalf("expected API error, got %v", err)
	}
}
