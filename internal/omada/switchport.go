// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"fmt"
)

// Switch ports are the provider's first **cross-surface** object: it reads them
// from the web API and writes them through the Open API.
//
// That split is not a design choice, it is what the controller offers. The web
// API serves the full port document at
//
//	GET /sites/{site}/switches/{mac}/ports
//
// but has no per-port write route the provider could use safely. The Open API
// has the write —
//
//	PATCH /openapi/v1/{omadacId}/sites/{site}/switches/{mac}/ports/{port}
//
// — and no matching read: the same path answers -1600 ("no such path") to a GET,
// as does /openapi/v1/.../switches and the paginated .../ports. Verified against
// a live v6.2 controller.
//
// So the read comes from one surface and the write from the other.
//
// v5.15 controllers differ: there the web API does have a per-port write,
//
//	PATCH /sites/{site}/switches/{mac}/ports/{port}
//
// verified with ES-series switches (ES206X-M2, ES210X-M2). It takes the whole
// port document rather than a partial one, so it is only used as a fallback when
// no Open API credentials are configured; see updateSwitchPortWebAPI.

// SwitchPort is one physical port on an adopted switch.
//
// The controller returns some 60 keys per port — PoE state, link telemetry,
// spanning tree, storm control, per-port QoS. Only the fields the Open API PATCH
// accepts are modelled, because those are the only ones this provider can write;
// see the note on Update below.
type SwitchPort struct {
	Port int    `json:"port"`
	Name string `json:"name"`

	// ProfileID is the port profile applied to this port (see
	// omada_port_profile). ProfileName is the controller's label for it and is
	// read-only — it is carried for diagnostics, not sent back.
	ProfileID   string `json:"profileId"`
	ProfileName string `json:"profileName"`

	// ProfileOverrideEnable turns on the per-port overrides below; with it
	// false the profile alone decides the port's VLAN membership.
	ProfileOverrideEnable     bool `json:"profileOverrideEnable"`
	ProfileVLANOverrideEnable bool `json:"profileVlanOverrideEnable"`

	NativeNetworkID    string   `json:"nativeNetworkId"`
	NetworkTagsSetting int      `json:"networkTagsSetting"`
	TagIDs             []string `json:"tagIds"`

	// TagName is the controller's label for the port tag in TagIDs, and is
	// read-only. It is modelled purely so the id is not opaque: a bare
	// `692c1fd42aa9224b41cc63e4` in a plan tells a reader nothing, while
	// "AP" alongside it explains the port at a glance.
	//
	// It also surfaces a case that is otherwise invisible. A tag deleted from
	// the site is *not* cleared from the ports referencing it: the id stays,
	// the controller keeps reporting the old name, and nothing in the UI shows
	// the dangling reference. Seeing both in state is how you notice.
	TagName string `json:"tagName"`

	// Duplex and LinkSpeed are the *configured* values, where 0 means
	// auto-negotiate. They are not the negotiated link state, which the
	// controller reports separately as `speed`.
	Duplex    int `json:"duplex"`
	LinkSpeed int `json:"linkSpeed"`
}

// switchPortUpdate is the Open API PATCH body: exactly the keys the controller
// accepts, and no others.
//
// This is a deliberate allow-list rather than a round-trip of the read document.
// Posting back the whole port would mean sending ~50 keys the endpoint does not
// document, several of them read-only telemetry, on a device carrying live
// traffic. Every field here was observed in the controller UI's own request.
type switchPortUpdate struct {
	Duplex                    int      `json:"duplex"`
	LinkSpeed                 int      `json:"linkSpeed"`
	Name                      string   `json:"name"`
	NativeNetworkID           string   `json:"nativeNetworkId"`
	NetworkTagsSetting        int      `json:"networkTagsSetting"`
	ProfileID                 string   `json:"profileId"`
	ProfileOverrideEnable     bool     `json:"profileOverrideEnable"`
	ProfileVLANOverrideEnable bool     `json:"profileVlanOverrideEnable"`
	TagIDs                    []string `json:"tagIds"`
}

func switchPortsPath(siteID, mac string) string {
	return fmt.Sprintf("/sites/%s/switches/%s/ports", siteID, NormalizeMAC(mac))
}

// ListSwitchPorts returns every port on a switch, read from the web API.
//
// Like the devices endpoint this answers a bare JSON array rather than the
// paginated {data,totalRows} envelope, so it is decoded directly.
func (c *Client) ListSwitchPorts(ctx context.Context, siteID, mac string) ([]SwitchPort, error) {
	var ports []SwitchPort
	if err := c.Do(ctx, "GET", switchPortsPath(siteID, mac), nil, &ports); err != nil {
		return nil, fmt.Errorf("listing ports on switch %s: %w", mac, err)
	}
	return ports, nil
}

// GetSwitchPort returns one port by its number.
//
// A missing port is an error rather than a nil result: ports are physical, so
// asking for one that does not exist means the port number is wrong or the MAC
// belongs to a different switch — neither is drift to be silently reconciled.
func (c *Client) GetSwitchPort(ctx context.Context, siteID, mac string, port int) (*SwitchPort, error) {
	ports, err := c.ListSwitchPorts(ctx, siteID, mac)
	if err != nil {
		return nil, err
	}
	for i := range ports {
		if ports[i].Port == port {
			return &ports[i], nil
		}
	}
	return nil, fmt.Errorf("switch %s has no port %d (it has %d ports)", mac, port, len(ports))
}

// UpdateSwitchPort writes a port's configuration through the Open API. Without
// Open API credentials a v5 controller is written through the web API instead
// (see updateSwitchPortWebAPI).
//
// Anything else without credentials returns ErrOpenAPINotConfigured, which is
// the common first failure — the admin username and password do not reach the
// Open API, and v6.2 refuses the web-API port write.
func (c *Client) UpdateSwitchPort(ctx context.Context, siteID, mac string, port int, p SwitchPort) error {
	if !c.openAPIConfigured() {
		v5, err := c.isV5(ctx)
		if err != nil {
			return fmt.Errorf("updating port %d on switch %s: %w", port, mac, err)
		}
		if v5 {
			return c.updateSwitchPortWebAPI(ctx, siteID, mac, port, p)
		}
	}
	body := switchPortUpdate{
		Duplex:                    p.Duplex,
		LinkSpeed:                 p.LinkSpeed,
		Name:                      p.Name,
		NativeNetworkID:           p.NativeNetworkID,
		NetworkTagsSetting:        p.NetworkTagsSetting,
		ProfileID:                 p.ProfileID,
		ProfileOverrideEnable:     p.ProfileOverrideEnable,
		ProfileVLANOverrideEnable: p.ProfileVLANOverrideEnable,
		TagIDs:                    nilToEmptyStrings(p.TagIDs),
	}
	path := c.OpenAPIPath(siteID, fmt.Sprintf("/switches/%s/ports/%d", NormalizeMAC(mac), port))
	if err := c.DoOpenAPI(ctx, "PATCH", path, body, nil); err != nil {
		return fmt.Errorf("updating port %d on switch %s: %w", port, mac, err)
	}
	return nil
}

// updateSwitchPortWebAPI writes a port through the web API:
//
//	PATCH /sites/{site}/switches/{mac}/ports/{port}
//
// The route rejects a partial body with "General error", so this is a
// read-modify-write: the port is fetched as an untyped map, only the fields this
// provider models are overwritten, and everything else (PoE, storm control, QoS)
// is sent back unchanged.
//
// A change made in the UI between the read and the write is lost. The Open API
// PATCH has no such window, which is why it stays the preferred path.
func (c *Client) updateSwitchPortWebAPI(ctx context.Context, siteID, mac string, port int, p SwitchPort) error {
	var docs []map[string]any
	if err := c.Do(ctx, "GET", switchPortsPath(siteID, mac), nil, &docs); err != nil {
		return fmt.Errorf("reading ports on switch %s before writing port %d: %w", mac, port, err)
	}

	doc := portDoc(docs, port)
	if doc == nil {
		return fmt.Errorf("switch %s has no port %d (it has %d ports)", mac, port, len(docs))
	}

	doc["duplex"] = p.Duplex
	doc["linkSpeed"] = p.LinkSpeed
	doc["name"] = p.Name
	doc["nativeNetworkId"] = p.NativeNetworkID
	doc["networkTagsSetting"] = p.NetworkTagsSetting
	doc["profileId"] = p.ProfileID
	doc["profileOverrideEnable"] = p.ProfileOverrideEnable
	doc["profileVlanOverrideEnable"] = p.ProfileVLANOverrideEnable
	doc["tagIds"] = nilToEmptyStrings(p.TagIDs)

	path := fmt.Sprintf("%s/%d", switchPortsPath(siteID, mac), port)
	if err := c.Do(ctx, "PATCH", path, doc, nil); err != nil {
		return fmt.Errorf("updating port %d on switch %s through the web API: %w", port, mac, err)
	}

	// v5.15 answers an unknown profileId with success and keeps the old
	// profile, so check what it stored.
	if err := c.Do(ctx, "GET", switchPortsPath(siteID, mac), nil, &docs); err != nil {
		return fmt.Errorf("reading port %d on switch %s after writing it: %w", port, mac, err)
	}
	if got := portDoc(docs, port); got == nil || got["profileId"] != p.ProfileID {
		return fmt.Errorf("switch %s kept its old profile on port %d; check that profile %q exists", mac, port, p.ProfileID)
	}
	return nil
}

// portDoc finds a port by number in the web API's port list.
func portDoc(docs []map[string]any, port int) map[string]any {
	for _, d := range docs {
		if n, ok := d["port"].(float64); ok && int(n) == port {
			return d
		}
	}
	return nil
}

// nilToEmptyStrings keeps a nil slice from marshalling as null, which the
// controller rejects where it expects a list.
func nilToEmptyStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
