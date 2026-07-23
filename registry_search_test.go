package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchRegistryJSONRejectsMalformedAndFailedResponses(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantError string
	}{
		{
			name:      "malformed success",
			status:    http.StatusOK,
			body:      "{",
			wantError: "decode response",
		},
		{
			name:      "provider message",
			status:    http.StatusBadGateway,
			body:      `{"message":"try later"}`,
			wantError: "try later",
		},
		{
			name:      "nested provider message",
			status:    http.StatusUnauthorized,
			body:      `{"error":{"message":"bad key"}}`,
			wantError: "bad key",
		},
		{
			name:      "unknown future error shape",
			status:    http.StatusTeapot,
			body:      `{"problem":"future"}`,
			wantError: "unexpected HTTP status 418 I'm a teapot",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(
				func(response http.ResponseWriter, _ *http.Request) {
					response.WriteHeader(test.status)
					_, _ = response.Write([]byte(test.body))
				},
			))
			defer server.Close()

			var result any
			err := fetchRegistryJSON(
				t.Context(),
				server.Client(),
				server.URL,
				"",
				&result,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}
