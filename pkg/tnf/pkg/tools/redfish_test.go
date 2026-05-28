package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetRedfishSystemUUID(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantUUID  string
		wantErr   bool
		checkAuth bool
		wantUser  string
		wantPass  string
	}{
		{
			name: "valid UUID",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(redfishSystemResponse{UUID: "a944436f-3d48-4139-8f3c-b0d397947966"})
			},
			wantUUID: "a944436f-3d48-4139-8f3c-b0d397947966",
		},
		{
			name: "basic auth is sent",
			handler: func(w http.ResponseWriter, r *http.Request) {
				user, pass, ok := r.BasicAuth()
				if !ok || user != "admin" || pass != "secret" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				json.NewEncoder(w).Encode(redfishSystemResponse{UUID: "test-uuid"})
			},
			checkAuth: true,
			wantUser:  "admin",
			wantPass:  "secret",
			wantUUID:  "test-uuid",
		},
		{
			name: "server error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "internal error", http.StatusInternalServerError)
			},
			wantErr: true,
		},
		{
			name: "malformed JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("not json"))
			},
			wantErr: true,
		},
		{
			name: "empty UUID",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(redfishSystemResponse{UUID: ""})
			},
			wantErr: true,
		},
		{
			name: "missing UUID field",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"Name": "System", "Status": "OK"}`))
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			user := "admin"
			pass := "password"
			if tt.checkAuth {
				user = tt.wantUser
				pass = tt.wantPass
			}

			got, err := getRedfishSystemUUID(context.Background(), server.URL, user, pass, false)
			if (err != nil) != tt.wantErr {
				t.Fatalf("getRedfishSystemUUID() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.wantUUID {
				t.Errorf("getRedfishSystemUUID() = %q, want %q", got, tt.wantUUID)
			}
		})
	}
}

func TestGetRedfishSystemUUIDContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(redfishSystemResponse{UUID: "test"})
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := getRedfishSystemUUID(ctx, server.URL, "admin", "pass", false)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestParseRedfishURL(t *testing.T) {
	tests := []struct {
		name    string
		address string
		want    string
		wantErr bool
	}{
		{
			name:    "redfish+https",
			address: "redfish+https://192.168.111.1:8000/redfish/v1/Systems/abc",
			want:    "https://192.168.111.1:8000/redfish/v1/Systems/abc",
		},
		{
			name:    "redfish+http",
			address: "redfish+http://host/path",
			want:    "http://host/path",
		},
		{
			name:    "plain https",
			address: "https://192.168.1.1:443/redfish/v1/Systems/1",
			want:    "https://192.168.1.1:443/redfish/v1/Systems/1",
		},
		{
			name:    "plain http",
			address: "http://host/path",
			want:    "http://host/path",
		},
		{
			name:    "no scheme",
			address: "192.168.1.1:8000/path",
			wantErr: true,
		},
		{
			name:    "unsupported scheme",
			address: "ftp://host/path",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRedfishURL(tt.address)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseRedfishURL(%q) error = %v, wantErr %v", tt.address, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseRedfishURL(%q) = %q, want %q", tt.address, got, tt.want)
			}
		})
	}
}
