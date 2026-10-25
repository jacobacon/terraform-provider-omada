// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newACLController serves a gateway ACL list built from acls and counts
// modifyIndex calls.
func newACLController(t *testing.T, acls []map[string]any, reorders *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"omadacId": "abc123"})
	})
	mux.HandleFunc("/abc123/api/v2/login", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"token": "tok-xyz"})
	})
	mux.HandleFunc("/abc123/api/v2/sites/site-1/setting/firewall/acls", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"totalRows": len(acls), "data": acls})
	})
	mux.HandleFunc("/abc123/api/v2/sites/site-1/cmd/acls/modifyIndex", func(w http.ResponseWriter, _ *http.Request) {
		*reorders++
		writeEnvelope(w, 0, "", nil)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestReorderACLs(t *testing.T) {
	cases := []struct {
		name        string
		acls        []map[string]any
		order       []string
		wantErr     string
		wantReorder int
	}{
		{
			name:        "moves rules among their positions",
			acls:        []map[string]any{{"id": "a", "index": 2}, {"id": "b", "index": 5}},
			order:       []string{"b", "a"},
			wantReorder: 1,
		},
		{
			name:  "already in order skips the write",
			acls:  []map[string]any{{"id": "a", "index": 2}, {"id": "b", "index": 5}},
			order: []string{"a", "b"},
		},
		{
			// Without the check this looked "already in order": no write and no error.
			name:    "missing index is refused, not reported as success",
			acls:    []map[string]any{{"id": "a"}, {"id": "b"}},
			order:   []string{"b", "a"},
			wantErr: "invalid rule positions",
		},
		{
			name:    "duplicate index is refused",
			acls:    []map[string]any{{"id": "a", "index": 3}, {"id": "b", "index": 3}},
			order:   []string{"b", "a"},
			wantErr: "invalid rule positions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reorders int
			srv := newACLController(t, tc.acls, &reorders)
			c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			err = c.ReorderACLs(context.Background(), "site-1", ACLTypeGateway, tc.order)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("ReorderACLs: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("ReorderACLs error = %v, want it to contain %q", err, tc.wantErr)
			}
			if reorders != tc.wantReorder {
				t.Fatalf("modifyIndex called %d times, want %d", reorders, tc.wantReorder)
			}
		})
	}
}

func TestCheckACLOrder(t *testing.T) {
	if err := CheckACLOrder([]ACL{{Index: 4}, {Index: 1}}); err != nil {
		t.Fatalf("distinct indexes: %v", err)
	}
	if err := CheckACLOrder([]ACL{{Index: 0}, {Index: 0}}); err == nil {
		t.Fatal("missing indexes: want error")
	}
}
