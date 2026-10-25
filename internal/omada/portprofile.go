// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"fmt"
)

// STPSetting is the spanning-tree sub-object of a port profile. `instances` is
// deliberately not modelled and is preserved on update.
type STPSetting struct {
	Priority    int  `json:"priority"`
	ExtPathCost int  `json:"extPathCost"`
	IntPathCost int  `json:"intPathCost"`
	EdgePort    bool `json:"edgePort"`
	P2PLink     int  `json:"p2pLink"`
	Mcheck      bool `json:"mcheck"`
	LoopProtect bool `json:"loopProtect"`
	RootProtect bool `json:"rootProtect"`
	TCGuard     bool `json:"tcGuard"`
	BPDUProtect bool `json:"bpduProtect"`
	BPDUFilter  bool `json:"bpduFilter"`
	BPDUForward bool `json:"bpduForward"`
}

// PortProfile is a switch port profile. Verified against a live v6.2 controller
// (/setting/lan/profiles). Read-only fields (prohibitModify, resource,
// networkConflict) are not modelled and are preserved on update; flag is read
// but never written.
type PortProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Flag says who made the profile: 0 for the built-ins (All, Default,
	// Disable), 1 for the one the controller creates with each VLAN network,
	// 2 for one made in the UI or through the API. Read-only.
	Flag               int      `json:"flag"`
	POE                int      `json:"poe"`
	VLANConfigEnable   bool     `json:"vlanConfigEnable"`
	NetworkTagsSetting int      `json:"networkTagsSetting"`
	NativeNetworkID    string   `json:"nativeNetworkId"`
	TagNetworkIDs      []string `json:"tagNetworkIds"`
	UntagNetworkIDs    []string `json:"untagNetworkIds"`

	VoiceNetworkEnable bool `json:"voiceNetworkEnable"`
	VoiceDscpEnable    bool `json:"voiceDscpEnable"`
	Dot1x              int  `json:"dot1x"`
	Dot1pPriority      int  `json:"dot1pPriority"`
	TrustMode          int  `json:"trustMode"`
	Type               int  `json:"type"`

	PortIsolationEnable     bool `json:"portIsolationEnable"`
	LLDPMedEnable           bool `json:"lldpMedEnable"`
	BandWidthCtrlType       int  `json:"bandWidthCtrlType"`
	LoopbackDetectEnable    bool `json:"loopbackDetectEnable"`
	LoopbackDetectVlanBased bool `json:"loopbackDetectVlanBasedEnable"`
	EEEEnable               bool `json:"eeeEnable"`
	FlowControlEnable       bool `json:"flowControlEnable"`
	FastLeaveEnable         bool `json:"fastLeaveEnable"`
	IGMPFastLeave           bool `json:"igmpFastLeaveEnable"`
	MLDFastLeave            bool `json:"mldFastLeaveEnable"`
	SupportESEnable         bool `json:"supportESEnable"`

	SpanningTreeEnable bool       `json:"spanningTreeEnable"`
	SpanningTree       STPSetting `json:"spanningTreeSetting"`
	DHCPL2Relay        EnableFlag `json:"dhcpL2RelaySettings"`
}

func profilesPath(siteID string) string {
	return fmt.Sprintf("/sites/%s/setting/lan/profiles", siteID)
}

// ListPortProfiles returns all port profiles for a site.
func (c *Client) ListPortProfiles(ctx context.Context, siteID string) ([]PortProfile, error) {
	return listAll[PortProfile](ctx, c, "port profiles", profilesPath(siteID))
}

// GetPortProfile returns one profile by id.
func (c *Client) GetPortProfile(ctx context.Context, siteID, id string) (*PortProfile, error) {
	profs, err := c.ListPortProfiles(ctx, siteID)
	if err != nil {
		return nil, err
	}
	for i := range profs {
		if profs[i].ID == id {
			return &profs[i], nil
		}
	}
	return nil, fmt.Errorf("port profile %q not found on site %q", id, siteID)
}

// profileCreateDefaults are fields a v5.15 controller will not default on
// create. Leaving any one out gets "-1001: must not be null", once per missing
// field and without naming it; this set was narrowed down by elimination.
//
// The values match the built-in "Default" profile. They are only sent to a v5
// controller, and only fill fields the caller left unset.
var profileCreateDefaults = map[string]any{
	"poe":                  2, // keep the device's own PoE setting
	"bandWidthCtrlType":    0,
	"spanningTreeEnable":   true,
	"portIsolationEnable":  false,
	"loopbackDetectEnable": false,
}

// CreatePortProfile creates a profile (null result → resolved by name).
func (c *Client) CreatePortProfile(ctx context.Context, siteID string, fields map[string]any) (*PortProfile, error) {
	name, _ := fields["name"].(string)
	v5, err := c.isV5(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating port profile: %w", err)
	}
	body := make(map[string]any, len(fields)+len(profileCreateDefaults))
	if v5 {
		for k, v := range profileCreateDefaults {
			body[k] = v
		}
	}
	for k, v := range fields {
		body[k] = v
	}
	if err := c.Do(ctx, "POST", profilesPath(siteID), body, nil); err != nil {
		return nil, fmt.Errorf("creating port profile: %w", err)
	}
	profs, err := c.ListPortProfiles(ctx, siteID)
	if err != nil {
		return nil, err
	}
	for i := range profs {
		if profs[i].Name == name {
			return &profs[i], nil
		}
	}
	return nil, fmt.Errorf("port profile %q not found after create", name)
}

// UpdatePortProfile does a read-modify-write, merging nested sub-objects
// (spanningTreeSetting, dhcpL2RelaySettings) so unmodelled keys such as the STP
// `instances` list survive.
func (c *Client) UpdatePortProfile(ctx context.Context, siteID, id string, fields map[string]any) (*PortProfile, error) {
	cur, err := c.RawByID(ctx, profilesPath(siteID), "id", id)
	if err != nil {
		return nil, err
	}
	mergeInto(cur, fields, "spanningTreeSetting", "dhcpL2RelaySettings")
	if err := c.Do(ctx, "PATCH", profilesPath(siteID)+"/"+id, cur, nil); err != nil {
		return nil, fmt.Errorf("updating port profile %q: %w", id, err)
	}
	return c.GetPortProfile(ctx, siteID, id)
}

// DeletePortProfile removes a profile.
func (c *Client) DeletePortProfile(ctx context.Context, siteID, id string) error {
	if err := c.Do(ctx, "DELETE", profilesPath(siteID)+"/"+id, nil, nil); err != nil {
		return fmt.Errorf("deleting port profile %q: %w", id, err)
	}
	return nil
}

// mergeInto overlays `fields` onto `cur`, deep-merging the named sub-object keys
// so keys we do not model inside them are preserved.
func mergeInto(cur, fields map[string]any, deepKeys ...string) {
	deep := map[string]bool{}
	for _, k := range deepKeys {
		deep[k] = true
	}
	for k, v := range fields {
		if deep[k] {
			nested, ok := v.(map[string]any)
			if !ok {
				cur[k] = v
				continue
			}
			base, _ := cur[k].(map[string]any)
			merged := map[string]any{}
			for bk, bv := range base {
				merged[bk] = bv
			}
			for nk, nv := range nested {
				merged[nk] = nv
			}
			cur[k] = merged
			continue
		}
		cur[k] = v
	}
}
