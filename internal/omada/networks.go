// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// EnableFlag is the common {"enable": bool} sub-object.
type EnableFlag struct {
	Enable bool `json:"enable"`
}

// NetworkIPv6RDNSS is the "Get from Prefix Delegation" sub-config: which WAN
// port supplies the delegation and which sub-prefix (preId) this network takes
// from it. Verified live with a WAN that delegates more than a single /64 —
// each network picks a distinct preId to get its own /64 out of the same
// delegation, e.g. two networks as preId 100 and 101.
type NetworkIPv6RDNSS struct {
	// PreType is a controller enum; 1 is the only value observed live.
	// TP-Link does not document the mapping.
	PreType  int    `json:"preType"`
	PortUUID string `json:"portUuid"`
	PreID    int    `json:"preId"`
	DNSv6    string `json:"dnsv6"` // e.g. "auto"
}

// NetworkIPv6RA is the Router Advertisement sub-config that accompanies SLAAC.
type NetworkIPv6RA struct {
	Enable            bool `json:"enable"`
	Preference        int  `json:"preference"`
	ValidLifetime     int  `json:"validLifetime"`
	PreferredLifetime int  `json:"preferredLifetime"`
}

// NetworkIPv6Config is lanNetworkIpv6Config. Verified live: a disabled network
// reports only {"enable": 0} — proto/rdnss/ra are absent entirely, not merely
// empty — while an enabled one carries proto plus the sub-object matching it
// ("rdnss" for SLAAC+RDNSS, confirmed live; other IPv6 Interface Type modes
// the controller UI offers — DHCPv6, SLAAC+Stateless DHCP, Pass-Through — are
// unverified and use payload shapes this type does not model).
//
// The asymmetric proto handling in UpdateNetwork (this file) exists because of
// this type: the web API's GET can return a bare {"enable": 0} that decodes
// here with Proto == "", but the PATCH rejects the object without a proto
// present at all.
type NetworkIPv6Config struct {
	Enable int               `json:"enable"`
	Proto  string            `json:"proto,omitempty"`
	RDNSS  *NetworkIPv6RDNSS `json:"rdnss,omitempty"`
	RA     *NetworkIPv6RA    `json:"ra,omitempty"`
}

// UnmarshalJSON tolerates the numeric proto placeholder that UpdateNetwork
// (below) sends to satisfy the controller's PATCH validation when a network
// is disabled: `proto` decodes as a string in real enabled state ("rdnss")
// but round-trips through a mocked/echoing PATCH as the literal int 0 sent
// for that placeholder. Either way, a non-string proto means "no mode" here.
func (c *NetworkIPv6Config) UnmarshalJSON(data []byte) error {
	type alias NetworkIPv6Config
	raw := struct {
		Proto json.RawMessage `json:"proto"`
		*alias
	}{alias: (*alias)(c)}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.Proto) > 0 {
		var s string
		if err := json.Unmarshal(raw.Proto, &s); err == nil {
			c.Proto = s
		}
	}
	return nil
}

// DHCPOption is a DHCP option handed out on a network (e.g. code 138 -> a
// controller/AP address).
type DHCPOption struct {
	Code  int    `json:"code"`
	Type  int    `json:"type"`
	Value string `json:"value"`
}

// DHCPSettings is the nested DHCP server config. Note: distinct from dhcpGuard,
// which is rogue-DHCP protection.
type DHCPSettings struct {
	Enable      bool         `json:"enable"`
	IPAddrStart string       `json:"ipaddrStart"`
	IPAddrEnd   string       `json:"ipaddrEnd"`
	LeaseTime   int          `json:"leasetime"`
	DNSMode     string       `json:"dhcpns"`
	Options     []DHCPOption `json:"options"`
}

// Network is a LAN network (VLAN interface) on a site. Field mappings verified
// against a live v6.2 controller (GET /sites/{id}/setting/lan/networks).
// Derived/read-only fields (ipRangeStart/End, ipRangePool, totalIpNum, state,
// deviceMac, ...) are intentionally not modelled; UpdateNetwork preserves them.
type Network struct {
	// Primary is true on the controller's built-in Default network, which
	// cannot be deleted. Read-only.
	Primary bool `json:"primary"`

	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Purpose       string   `json:"purpose"` // "interface" | "vlan"
	VLANID        int      `json:"vlan"`
	VLANType      int      `json:"vlanType"`
	Application   int      `json:"application"`
	GatewaySubnet string   `json:"gatewaySubnet"`
	InterfaceIDs  []string `json:"interfaceIds"`

	Isolation         bool `json:"isolation"`
	AllLan            bool `json:"allLan"`
	Portal            bool `json:"portal"`
	RateLimit         bool `json:"rateLimit"`
	QosQueueEnable    bool `json:"qosQueueEnable"`
	AccessControlRule bool `json:"accessControlRule"`
	ArpDetection      bool `json:"arpDetectionEnable"`
	IGMPSnoop         bool `json:"igmpSnoopEnable"`
	FastLeave         bool `json:"fastLeaveEnable"`
	MLDSnoop          bool `json:"mldSnoopEnable"`
	DHCPL2Relay       bool `json:"dhcpL2RelayEnable"`

	DHCPGuard    EnableFlag        `json:"dhcpGuard"`
	DHCPv6Guard  EnableFlag        `json:"dhcpv6Guard"`
	IPv6Config   NetworkIPv6Config `json:"lanNetworkIpv6Config"`
	DHCPSettings DHCPSettings      `json:"dhcpSettings"`
}

// DHCPEnabled reports whether the DHCP server is enabled on this network.
func (n Network) DHCPEnabled() bool { return n.DHCPSettings.Enable }

func networksPath(siteID string) string {
	return fmt.Sprintf("/sites/%s/setting/lan/networks", siteID)
}

// CreateNetwork creates a LAN network.
//
// An L2-only "vlan" network goes through the web API instead — see
// createVLANNetwork — because a gateway-less site cannot reach the Open API
// create at all. Every other purpose still goes through the Open API below:
// on a site with a gateway, POSTing to /setting/lan/networks is rejected
// outright, so "interface" creates live there instead:
//
//	POST /openapi/v2/{omadacId}/sites/{site}/lan-networks
//
// (v1 exists too but demands a longer required-field list for no benefit.)
//
// **Only the four fields the endpoint requires are sent here**, and everything
// else the practitioner configured is applied straight afterwards by the
// ordinary web-API update. That split is deliberate. The two surfaces describe
// a network differently — the Open API nests DHCP under `dhcpSettingsVO`, the
// web API uses `dhcpSettings` — so translating the full configuration into Open
// API shape would mean maintaining a second, largely untested mapping of every
// field, and a mistake in it would land on a live VLAN. Creating a minimal
// network and then updating it through the code path that is already exercised
// on every apply is both less code and better tested.
//
// The consequence worth knowing: creating a network is two calls, so an
// interruption between them can leave a network that exists but is not yet
// fully configured. It will be reconciled on the next apply.
func (c *Client) CreateNetwork(ctx context.Context, siteID string, fields map[string]any) (*Network, error) {
	name, _ := fields["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("creating network: name is required")
	}

	// An L2-only VLAN goes through the web API instead, and needs neither Open
	// API credentials nor a gateway. See createVLANNetwork.
	if isVLANPurpose(fields["purpose"]) {
		return c.createVLANNetwork(ctx, siteID, name, fields)
	}

	if !c.openAPIConfigured() {
		return nil, fmt.Errorf("creating a network needs Open API credentials: %w", ErrOpenAPINotConfigured)
	}

	purpose, err := openAPIPurpose(fields["purpose"])
	if err != nil {
		return nil, fmt.Errorf("creating network %q: %w", name, err)
	}

	seed := map[string]any{
		"name":            name,
		"purpose":         purpose,
		"vlan":            fields["vlan"],
		"igmpSnoopEnable": valueOr(fields, "igmpSnoopEnable", false),
		"interfaceIds":    fields["interfaceIds"],
		"gatewaySubnet":   fields["gatewaySubnet"],
	}
	if seed["vlan"] == nil {
		return nil, fmt.Errorf("creating network %q: vlan is required", name)
	}
	// Required whenever purpose is "interface" (-35930), which is every network
	// this provider can currently create.
	if sub, _ := seed["gatewaySubnet"].(string); sub == "" {
		return nil, fmt.Errorf("creating network %q: gateway_subnet is required", name)
	}
	// The controller rejects a network bound to nothing (-33515 "LAN interfaces
	// could not be none"), so this cannot be deferred to the update call the way
	// the rest of the configuration is. It is checked here to give a better
	// message than the raw code, and because the check is cheap.
	if ifaces, ok := seed["interfaceIds"].([]string); !ok || len(ifaces) == 0 {
		return nil, fmt.Errorf("creating network %q: interface_ids must list at least one LAN "+
			"interface — the controller refuses a network that is not bound to one (-33515). "+
			"Copy the ids from an existing network via the omada_networks data source", name)
	}

	path := c.OpenAPIPathVersion(2, siteID, "/lan-networks")
	if err := c.DoOpenAPI(ctx, "POST", path, seed, nil); err != nil {
		return nil, fmt.Errorf("creating network %q: %w", name, err)
	}

	// The create response carries no id, so the new network is located by name.
	created, err := c.getNetworkByName(ctx, siteID, name)
	if err != nil {
		return nil, fmt.Errorf("network %q was created but could not be read back: %w", name, err)
	}

	// Apply the rest of the configuration on the surface that supports it.
	rest := map[string]any{}
	for k, v := range fields {
		if _, seeded := seed[k]; !seeded {
			rest[k] = v
		}
	}
	if len(rest) == 0 {
		return created, nil
	}
	updated, err := c.UpdateNetwork(ctx, siteID, created.ID, rest)
	if err != nil {
		return nil, fmt.Errorf("network %q was created but its configuration could not be applied "+
			"(it exists on the controller and will be reconciled on the next apply): %w", name, err)
	}
	return updated, nil
}

// openAPIPurpose translates a network purpose from the web API's spelling to the
// Open API's.
//
// The two surfaces disagree: the web API calls it "interface", the Open API
// calls it 1. That mismatch is the whole reason create seeds a minimal network
// and lets the web-API update apply the rest — every field carried across is a
// translation that has to be right, and this one silently produced
// "-1001 Invalid request parameters" until it was found.
//
// Only "interface" is mapped, because it is the only value observed on live
// hardware. An unknown purpose is an error rather than a guess: guessing here
// would create a network of the wrong kind.
func openAPIPurpose(v any) (int, error) {
	switch p := v.(type) {
	case nil:
		return 1, nil
	case int:
		return p, nil
	case string:
		if p == "" || p == "interface" {
			return 1, nil
		}
		return 0, fmt.Errorf("purpose %q has no known Open API equivalent — only %q is mapped. "+
			"(%q networks do not come through here at all; they are created on the web API by "+
			"createVLANNetwork.) Create it in the controller UI and import it instead",
			p, "interface", "vlan")
	default:
		return 0, fmt.Errorf("unexpected purpose type %T", v)
	}
}

// isVLANPurpose reports whether these fields describe an L2-only VLAN.
//
// The web API spells purposes as strings; "vlan" is the one the Open API has no
// number for.
func isVLANPurpose(v any) bool {
	s, _ := v.(string)
	return s == "vlan"
}

// createVLANNetwork creates an L2-only VLAN through the WEB API.
//
//	POST /sites/{site}/setting/lan/networks
//
// This is the exception to "create is the one network operation the web API
// will not do". That claim came from a site with an ER707-M2 gateway, where the
// POST is refused; it is not true everywhere. On a site with **no gateway**
// (verified on v6.2.14.11, a site whose only device was an EAP670) the same
// POST creates a VLAN network from four fields:
//
//	{"name": ..., "vlan": 1-4094, "purpose": "vlan", "igmpSnoopEnable": false}
//
// and answers errorCode 0 with the new id as a bare JSON string.
//
// Neither gatewaySubnet nor interfaceIds is sent, and neither may be: on this
// path the controller owns both. The created network came back with
// interfaceIds already populated with four ids and allLan true, and
// gatewaySubnet absent — which is the shape an L2 VLAN should have, since with
// no gateway there is no subnet to route and no LAN interface for the caller to
// name. The Open API's two refusals that block this path (-35930 "gatewaySubnet
// required" and -33515 "LAN interfaces could not be none") are therefore rules
// of gateway sites, not of networks in general.
//
// # Why this is not just used for every network
//
// Because the refusal on gateway sites is real and documented, and an
// interface-purpose network genuinely does need the subnet and interfaces the
// Open API asks for. Routing by purpose keeps the proven path proven: nothing
// about creating an "interface" network changes.
//
// # How the contract was established without creating anything
//
// Every probe carried vlan 99999, outside the legal 1-4094, so validation could
// never let one through (DESIGN.md §5.1). This endpoint reports every
// unsatisfied field at once rather than short-circuiting, so the required set
// could be walked out in two rounds. One real create on a throwaway VLAN then
// confirmed no further round of validation was hiding behind the range check,
// and the object was deleted again.
func (c *Client) createVLANNetwork(ctx context.Context, siteID, name string, fields map[string]any) (*Network, error) {
	if fields["vlan"] == nil {
		return nil, fmt.Errorf("creating network %q: vlan is required", name)
	}

	// Both are controller-owned here. Sending a caller's value would either be
	// ignored or produce a network that claims a subnet it cannot route, so say
	// so rather than quietly dropping them.
	for field, attr := range map[string]string{
		"gatewaySubnet": "gateway_subnet",
		"interfaceIds":  "interface_ids",
	} {
		// Presence alone, not isEmptyValue: an explicit empty value
		// (interface_ids = [] or gateway_subnet = "") is a known, non-null
		// value distinct from omitting the attribute. isEmptyValue would let
		// it slip through here, fall into rest below, and get PATCHed onto
		// the controller-assigned interfaceIds/gatewaySubnet by UpdateNetwork's
		// read-modify-write — silently clobbering what this refusal exists to
		// protect.
		if _, ok := fields[field]; ok {
			return nil, fmt.Errorf("creating network %q: %s cannot be set on a %q network — "+
				"the controller assigns it (an L2-only VLAN has no subnet to route and no "+
				"gateway interface to bind to). Remove it, or use purpose %q",
				name, attr, "vlan", "interface")
		}
	}

	seed := map[string]any{
		"name":            name,
		"vlan":            fields["vlan"],
		"purpose":         "vlan",
		"igmpSnoopEnable": valueOr(fields, "igmpSnoopEnable", false),
	}

	// Unlike the Open API create, this one returns the new id, so there is no
	// resolve-by-name step and none of §5.4a's ambiguity about which network
	// was just made.
	var id string
	if err := c.Do(ctx, http.MethodPost, networksPath(siteID), seed, &id); err != nil {
		return nil, fmt.Errorf("creating vlan network %q: %w", name, err)
	}
	if id == "" {
		return nil, fmt.Errorf("creating vlan network %q: controller reported success but "+
			"returned no id", name)
	}

	// Everything else is applied by the ordinary update, exactly as the Open API
	// path does — same reasoning, and the same consequence: an interruption
	// between the two calls leaves a network that exists but is not yet fully
	// configured, reconciled on the next apply.
	rest := map[string]any{}
	for k, v := range fields {
		if _, seeded := seed[k]; !seeded {
			rest[k] = v
		}
	}
	if len(rest) == 0 {
		return c.GetNetwork(ctx, siteID, id)
	}
	updated, err := c.UpdateNetwork(ctx, siteID, id, rest)
	if err != nil {
		return nil, fmt.Errorf("vlan network %q was created but its configuration could not be "+
			"applied (it exists on the controller and will be reconciled on the next apply): %w",
			name, err)
	}
	return updated, nil
}

// valueOr returns fields[key], or def when it is absent or nil.
func valueOr(fields map[string]any, key string, def any) any {
	if v, ok := fields[key]; ok && v != nil {
		return v
	}
	return def
}

// GetNetwork returns a single network by id. The controller has no single-object
// GET for LAN networks, so this filters the (validated) list endpoint.
func (c *Client) GetNetwork(ctx context.Context, siteID, id string) (*Network, error) {
	nets, err := c.ListNetworks(ctx, siteID)
	if err != nil {
		return nil, err
	}
	for i := range nets {
		if nets[i].ID == id {
			return &nets[i], nil
		}
	}
	return nil, fmt.Errorf("network %q not found on site %q", id, siteID)
}

func (c *Client) getNetworkByName(ctx context.Context, siteID, name string) (*Network, error) {
	nets, err := c.ListNetworks(ctx, siteID)
	if err != nil {
		return nil, err
	}
	for i := range nets {
		if nets[i].Name == name {
			return &nets[i], nil
		}
	}
	return nil, fmt.Errorf("network %q not found on site %q after create", name, siteID)
}

// UpdateNetwork does a read-modify-write: fetch the current object, overlay the
// managed fields, and PATCH the whole thing back so derived/unmodelled fields
// (ipRangePool, totalIpNum, ...) survive.
func (c *Client) UpdateNetwork(ctx context.Context, siteID, id string, fields map[string]any) (*Network, error) {
	cur, err := c.RawByID(ctx, networksPath(siteID), "id", id)
	if err != nil {
		return nil, err
	}
	for k, v := range fields {
		// Nested objects are merged, never replaced. The controller owns keys
		// inside them that this provider does not model — `ipRangePool` under
		// dhcpSettings, `proto` under lanNetworkIpv6Config — and sending a
		// replacement object drops them. Dropping `proto` is rejected outright
		// (`-1001 Parameter [proto] should not be null`); dropping the DHCP
		// pool would silently discard the address range.
		//
		// This was originally special-cased for dhcpSettings alone, which meant
		// every other nested object still clobbered. Merging by shape rather
		// than by name fixes the ones not yet discovered too.
		if incoming, ok := v.(map[string]any); ok {
			if base, ok := cur[k].(map[string]any); ok {
				merged := make(map[string]any, len(base)+len(incoming))
				for bk, bv := range base {
					merged[bk] = bv
				}
				for nk, nv := range incoming {
					merged[nk] = nv
				}
				cur[k] = merged
				continue
			}
		}
		cur[k] = v
	}
	// The IPv6 block is asymmetric: the web API's GET returns it as `{enable}`
	// only, but its PATCH rejects the object unless `proto` is present
	// (`-1001 Parameter [proto] should not be null`). A read-modify-write cannot
	// restore a key the read never returned, so the default is supplied here.
	// Zero is not a guess — the Open API, which does return the field, reports
	// `proto: 0` for every network on the site — and dropping the block instead
	// is not an option: without it the controller answers `-1 General error`.
	if v6, ok := cur["lanNetworkIpv6Config"].(map[string]any); ok {
		if _, has := v6["proto"]; !has {
			v6["proto"] = 0
		}
	}

	if err := c.Do(ctx, "PATCH", networksPath(siteID)+"/"+id, cur, nil); err != nil {
		return nil, fmt.Errorf("updating network %q: %w", id, err)
	}
	return c.GetNetwork(ctx, siteID, id)
}

// DeleteNetwork removes a network.
func (c *Client) DeleteNetwork(ctx context.Context, siteID, id string) error {
	if err := c.Do(ctx, "DELETE", networksPath(siteID)+"/"+id, nil, nil); err != nil {
		return fmt.Errorf("deleting network %q: %w", id, err)
	}
	return nil
}

// ListNetworks returns every LAN network for the given site, following pagination.
func (c *Client) ListNetworks(ctx context.Context, siteID string) ([]Network, error) {
	return listAll[Network](ctx, c, "networks", networksPath(siteID))
}
