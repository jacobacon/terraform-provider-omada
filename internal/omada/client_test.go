// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
)

// newTestController spins up an httptest server that mimics the Omada
// info + login handshake and a sites list, so the client can be exercised
// without a real controller. As real endpoints are reverse-engineered, extend
// this fixture (or the stateful mock in the provider package) with their shapes.
func newTestController(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{
			"omadacId":      "abc123",
			"controllerVer": "6.0.0",
			"apiVer":        "3",
			"type":          1,
		})
	})

	mux.HandleFunc("/abc123/api/v2/login", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"token": "tok-xyz"})
	})

	mux.HandleFunc("/abc123/api/v2/sites", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Csrf-Token"); got != "tok-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			writeEnvelope(w, -1400, "invalid csrf token", nil)
			return
		}
		writeEnvelope(w, 0, "", map[string]any{
			"totalRows":   1,
			"currentPage": 1,
			"currentSize": 100,
			"data":        []map[string]any{{"id": "site-1", "name": "Default"}},
		})
	})

	// Access point device document: PATCH is a partial update on the real
	// controller, so the fixture only has to accept a body and succeed.
	mux.HandleFunc("/abc123/api/v2/sites/site-1/eaps/AA-BB-CC-DD-EE-FF", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Csrf-Token"); got != "tok-xyz" {
			writeEnvelope(w, -1400, "invalid csrf token", nil)
			return
		}
		writeEnvelope(w, 0, "", map[string]any{
			"mac":  "AA-BB-CC-DD-EE-FF",
			"name": "test-ap",
		})
	})

	mux.HandleFunc("/abc123/api/v2/sites/site-1/setting/lan/networks", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Csrf-Token"); got != "tok-xyz" {
			writeEnvelope(w, -1400, "invalid csrf token", nil)
			return
		}
		writeEnvelope(w, 0, "", map[string]any{
			"totalRows":   1,
			"currentPage": 1,
			"currentSize": 100,
			"data": []map[string]any{{
				"id": "net-1", "name": "IoT", "purpose": "interface", "primary": true,
				"vlan": 30, "gatewaySubnet": "192.168.30.1/24",
				"dhcpSettings": map[string]any{"enable": true, "ipaddrStart": "192.168.30.2", "ipaddrEnd": "192.168.30.254", "leasetime": 120},
			}},
		})
	})

	return httptest.NewServer(mux)
}

func writeEnvelope(w http.ResponseWriter, code int, msg string, result any) {
	var raw json.RawMessage
	if result != nil {
		raw, _ = json.Marshal(result)
	}
	_ = json.NewEncoder(w).Encode(APIResponse{ErrorCode: code, Msg: msg, Result: raw})
}

func TestClientLoginAndListSites(t *testing.T) {
	srv := newTestController(t)
	defer srv.Close()

	c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	sites, err := c.ListSites(context.Background())
	if err != nil {
		t.Fatalf("ListSites: %v", err)
	}
	if len(sites) != 1 || sites[0].Name != "Default" || sites[0].ID != "site-1" {
		t.Fatalf("unexpected sites: %+v", sites)
	}

	id, err := c.ResolveSiteID(context.Background(), "Default")
	if err != nil {
		t.Fatalf("ResolveSiteID: %v", err)
	}
	if id != "site-1" {
		t.Fatalf("ResolveSiteID = %q, want site-1", id)
	}
}

func TestClientListNetworks(t *testing.T) {
	srv := newTestController(t)
	defer srv.Close()

	c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	nets, err := c.ListNetworks(context.Background(), "site-1")
	if err != nil {
		t.Fatalf("ListNetworks: %v", err)
	}
	if len(nets) != 1 {
		t.Fatalf("got %d networks, want 1", len(nets))
	}
	n := nets[0]
	if n.ID != "net-1" || n.Name != "IoT" || n.VLANID != 30 ||
		n.GatewaySubnet != "192.168.30.1/24" || !n.DHCPEnabled() || !n.Primary {
		t.Fatalf("unexpected network: %+v", n)
	}
}

func TestPageURL(t *testing.T) {
	cases := []struct{ base, want string }{
		{"/sites", "/sites?currentPage=2&currentPageSize=100"},
		{"/x/acls?type=1", "/x/acls?type=1&currentPage=2&currentPageSize=100"},
	}
	for _, c := range cases {
		if got := pageURL(c.base, 2, 100); got != c.want {
			t.Errorf("pageURL(%q) = %q, want %q", c.base, got, c.want)
		}
	}
}

// TestListAllPagination proves the client walks every page of a list endpoint,
// not just the first — the mock here honours currentPage and serves the 250
// items across three pages (100 + 100 + 50).
func TestListAllPagination(t *testing.T) {
	const total = 250
	var pagesServed []int

	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"omadacId": "abc123"})
	})
	mux.HandleFunc("/abc123/api/v2/login", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"token": "tok-xyz"})
	})
	mux.HandleFunc("/abc123/api/v2/sites", func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("currentPage"))
		size, _ := strconv.Atoi(r.URL.Query().Get("currentPageSize"))
		pagesServed = append(pagesServed, page)
		start := (page - 1) * size
		data := []map[string]any{}
		for i := start; i < start+size && i < total; i++ {
			data = append(data, map[string]any{"id": fmt.Sprintf("site-%d", i), "name": "s"})
		}
		writeEnvelope(w, 0, "", map[string]any{"totalRows": total, "data": data})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	sites, err := c.ListSites(context.Background())
	if err != nil {
		t.Fatalf("ListSites: %v", err)
	}
	if len(sites) != total {
		t.Fatalf("got %d sites, want %d", len(sites), total)
	}
	if want := []int{1, 2, 3}; !slices.Equal(pagesServed, want) {
		t.Fatalf("pages requested = %v, want %v", pagesServed, want)
	}
	if sites[0].ID != "site-0" || sites[total-1].ID != fmt.Sprintf("site-%d", total-1) {
		t.Fatalf("wrong ordering/content: first=%s last=%s", sites[0].ID, sites[total-1].ID)
	}
}

// TestClientReloginOnExpiredSession verifies the client transparently
// re-authenticates once when the controller reports an expired session.
func TestClientReloginOnExpiredSession(t *testing.T) {
	var loginCount, expiredServed int
	mux := http.NewServeMux()

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"omadacId": "abc123"})
	})
	mux.HandleFunc("/abc123/api/v2/login", func(w http.ResponseWriter, _ *http.Request) {
		loginCount++
		writeEnvelope(w, 0, "", map[string]any{"token": "tok-xyz"})
	})
	mux.HandleFunc("/abc123/api/v2/sites", func(w http.ResponseWriter, _ *http.Request) {
		// First call: pretend the session expired. Second call: succeed.
		if expiredServed == 0 {
			expiredServed++
			writeEnvelope(w, -1200, "session expired", nil)
			return
		}
		writeEnvelope(w, 0, "", map[string]any{
			"totalRows": 1, "currentPage": 1, "currentSize": 100,
			"data": []map[string]any{{"id": "site-1", "name": "Default"}},
		})
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.ListSites(context.Background()); err != nil {
		t.Fatalf("ListSites after expiry: %v", err)
	}
	if loginCount != 2 {
		t.Fatalf("expected 2 logins (initial + re-login), got %d", loginCount)
	}
}
