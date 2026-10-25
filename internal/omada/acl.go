// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"fmt"
	"sort"
)

// ACL type values.
const (
	ACLTypeGateway = 0
	ACLTypeSwitch  = 1
	ACLTypeEAP     = 2
)

// ACLDirection is the traffic direction for a gateway ACL.
type ACLDirection struct {
	LanToWan bool     `json:"lanToWan"`
	LanToLan bool     `json:"lanToLan"`
	WanInIDs []string `json:"wanInIds"`
	VpnInIDs []string `json:"vpnInIds"`
}

// ACL is a firewall ACL rule. Verified against a live v6.2 controller
// (/setting/firewall/acls?type=N). Create returns null; update uses PUT.
type ACL struct {
	ID string `json:"id"`
	// Index is the rule's 1-based position in its type's evaluation order.
	// Read-only in practice: it comes back on the list, but setting it through
	// the rule's own PUT is accepted and silently ignored (see ReorderACLs).
	Index           int          `json:"index"`
	Type            int          `json:"type"`
	Name            string       `json:"name"`
	Status          bool         `json:"status"`
	Policy          int          `json:"policy"` // 0=deny, 1=permit
	Protocols       []int        `json:"protocols"`
	SourceType      int          `json:"sourceType"`
	SourceIDs       []string     `json:"sourceIds"`
	DestinationType int          `json:"destinationType"`
	DestinationIDs  []string     `json:"destinationIds"`
	Direction       ACLDirection `json:"direction"`
}

// ACLInput is the create/update payload. customAclPorts/customAclDevices are
// sent empty (not yet modelled); ID is set for PUT updates.
type ACLInput struct {
	ID               string       `json:"id,omitempty"`
	Type             int          `json:"type"`
	Name             string       `json:"name"`
	Status           bool         `json:"status"`
	Policy           int          `json:"policy"`
	Protocols        []int        `json:"protocols"`
	SourceType       int          `json:"sourceType"`
	SourceIDs        []string     `json:"sourceIds"`
	DestinationType  int          `json:"destinationType"`
	DestinationIDs   []string     `json:"destinationIds"`
	Direction        ACLDirection `json:"direction"`
	CustomACLPorts   []any        `json:"customAclPorts"`
	CustomACLDevices []any        `json:"customAclDevices"`
	StateMode        int          `json:"stateMode"`
	Syslog           bool         `json:"syslog"`
}

func aclsPath(siteID string) string {
	return fmt.Sprintf("/sites/%s/setting/firewall/acls", siteID)
}

// ListACLs returns the ACL rules of a given type for a site.
func (c *Client) ListACLs(ctx context.Context, siteID string, aclType int) ([]ACL, error) {
	base := fmt.Sprintf("%s?type=%d", aclsPath(siteID), aclType)
	return listAll[ACL](ctx, c, "acls", base)
}

// GetACL returns a single ACL by id (within a type).
func (c *Client) GetACL(ctx context.Context, siteID string, aclType int, id string) (*ACL, error) {
	acls, err := c.ListACLs(ctx, siteID, aclType)
	if err != nil {
		return nil, err
	}
	for i := range acls {
		if acls[i].ID == id {
			return &acls[i], nil
		}
	}
	return nil, fmt.Errorf("acl %q not found on site %q", id, siteID)
}

// CreateACL creates a rule.
//
// The POST returns a null result, so the new rule is resolved by name from the
// list that follows. That is safe here, and the reason is worth writing down
// because it is not obvious from this code: **the controller enforces unique
// ACL names site-wide.** Creating a second rule with an existing name is
// refused with -33006 "This name already exists in this site", confirmed
// against a live controller.
//
// So the name match below cannot be ambiguous. Adding duplicate-detection
// around it would be dead code — and worse than dead: any such check has to
// run *after* the POST has already succeeded, so failing there would orphan
// the rule it just created, and the next apply would then hit -33006 forever
// because the orphan owns the name. See CreateIoTServer for the endpoint where
// post-create recovery genuinely is needed, and how it is done.
func (c *Client) CreateACL(ctx context.Context, siteID string, in *ACLInput) (*ACL, error) {
	if in.CustomACLPorts == nil {
		in.CustomACLPorts = []any{}
	}
	if in.CustomACLDevices == nil {
		in.CustomACLDevices = []any{}
	}
	if err := c.Do(ctx, "POST", aclsPath(siteID), in, nil); err != nil {
		return nil, fmt.Errorf("creating acl: %w", err)
	}
	acls, err := c.ListACLs(ctx, siteID, in.Type)
	if err != nil {
		return nil, err
	}
	for i := range acls {
		if acls[i].Name == in.Name {
			return &acls[i], nil
		}
	}
	return nil, fmt.Errorf("acl %q not found after create", in.Name)
}

// UpdateACL replaces a rule (PUT — PATCH is unsupported here).
func (c *Client) UpdateACL(ctx context.Context, siteID, id string, in *ACLInput) (*ACL, error) {
	in.ID = id
	if in.CustomACLPorts == nil {
		in.CustomACLPorts = []any{}
	}
	if in.CustomACLDevices == nil {
		in.CustomACLDevices = []any{}
	}
	if err := c.Do(ctx, "PUT", aclsPath(siteID)+"/"+id, in, nil); err != nil {
		return nil, fmt.Errorf("updating acl %q: %w", id, err)
	}
	return c.GetACL(ctx, siteID, in.Type, id)
}

// DeleteACL removes a rule.
func (c *Client) DeleteACL(ctx context.Context, siteID, id string) error {
	if err := c.Do(ctx, "DELETE", aclsPath(siteID)+"/"+id, nil, nil); err != nil {
		return fmt.Errorf("deleting acl %q: %w", id, err)
	}
	return nil
}

// aclReorderPath is under /cmd/, not /setting/ — the only such path in this
// client. Nothing under /setting/firewall/acls reorders anything, which is why
// a sweep of that subtree makes ordering look unsupported.
func aclReorderPath(siteID string) string {
	return fmt.Sprintf("/sites/%s/cmd/acls/modifyIndex", siteID)
}

// aclReorderInput is the modifyIndex payload: an ACL type plus a map of rule id
// to its new 1-based index.
type aclReorderInput struct {
	Type    int            `json:"type"`
	Indexes map[string]int `json:"indexes"`
}

// ReorderACLs arranges orderedIDs into that relative order, in place.
//
// **The endpoint is a PERMUTATION, not an insert.** The set of indexes sent must
// equal the set of indexes the named rules already hold, so this function keeps
// those positions and redistributes them in the requested order: rules at
// positions 2, 5 and 7 can be rearranged among 2, 5 and 7, and nothing else on
// the controller moves. Sending an index a named rule does not currently hold is
// refused with -1001 "Invalid request parameters.".
//
// That is why there is no "move rule X to position N" call here. To move a rule
// past a rule the caller has not named, the caller must name that rule too —
// passing every rule of the type gives full control of the order.
//
// Verified against a live v6.2 controller (2026-10-20). The controller validates
// the whole payload before applying any of it: an unknown rule id, a duplicate
// index, or an out-of-range index all return -1001 with nothing moved, so a
// rejected call cannot leave a half-applied ordering.
func (c *Client) ReorderACLs(ctx context.Context, siteID string, aclType int, orderedIDs []string) error {
	if len(orderedIDs) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(orderedIDs))
	for _, id := range orderedIDs {
		if seen[id] {
			return fmt.Errorf("duplicate acl id %q in the requested order", id)
		}
		seen[id] = true
	}

	acls, err := c.ListACLs(ctx, siteID, aclType)
	if err != nil {
		return err
	}
	current := make(map[string]int, len(acls))
	for _, a := range acls {
		current[a.ID] = a.Index
	}

	// Collect the positions the named rules occupy; those are the only values
	// the controller will accept back.
	positions := make([]int, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		idx, ok := current[id]
		if !ok {
			return fmt.Errorf("acl %q not found on site %q (type %d)", id, siteID, aclType)
		}
		positions = append(positions, idx)
	}
	sort.Ints(positions)
	if err := checkACLPositions(positions); err != nil {
		return fmt.Errorf("acls %v on site %q (type %d): %w", orderedIDs, siteID, aclType, err)
	}

	indexes := make(map[string]int, len(orderedIDs))
	changed := false
	for i, id := range orderedIDs {
		indexes[id] = positions[i]
		if current[id] != positions[i] {
			changed = true
		}
	}
	if !changed {
		return nil // already in this order; skip the write entirely
	}

	in := aclReorderInput{Type: aclType, Indexes: indexes}
	if err := c.Do(ctx, "POST", aclReorderPath(siteID), in, nil); err != nil {
		return fmt.Errorf("reordering acls on site %q: %w", siteID, err)
	}
	return nil
}

// checkACLPositions rejects a sorted set of rule indexes that are not distinct
// and 1-based. A list response without `index` decodes every rule to 0, which
// would otherwise make any order look already applied: the write is skipped and
// a reorder that never happened is reported as a success.
func checkACLPositions(sorted []int) error {
	for i, p := range sorted {
		if p < 1 || (i > 0 && p == sorted[i-1]) {
			return fmt.Errorf("controller reported invalid rule positions %v; expected distinct 1-based indexes", sorted)
		}
	}
	return nil
}

// CheckACLOrder validates the Index of each rule in acls the same way
// ReorderACLs does, for callers that read an order back.
func CheckACLOrder(acls []ACL) error {
	positions := make([]int, 0, len(acls))
	for _, a := range acls {
		positions = append(positions, a.Index)
	}
	sort.Ints(positions)
	return checkACLPositions(positions)
}
