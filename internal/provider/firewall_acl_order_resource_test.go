// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Three rules, created in an order that is deliberately WRONG: the deny is
// created before the permits, so it appends above them and shadows both. That is
// the real-world failure omada_firewall_acl_order exists to fix.
const aclOrderRules = `
resource "omada_firewall_acl" "deny_wan" {
  name            = "Deny Cameras to WAN"
  policy          = "deny"
  source_ids      = ["net-cameras"]
  destination_ids = ["grp-any"]
  direction       = { lan_to_lan = false, lan_to_wan = true }
}

resource "omada_firewall_acl" "permit_a" {
  name            = "Permit Frigate to WAN"
  policy          = "permit"
  source_ids      = ["grp-frigate"]
  destination_ids = ["grp-any"]
  direction       = { lan_to_lan = false, lan_to_wan = true }
  depends_on      = [omada_firewall_acl.deny_wan]
}

resource "omada_firewall_acl" "permit_b" {
  name            = "Permit NVR to WAN"
  policy          = "permit"
  source_ids      = ["grp-nvr"]
  destination_ids = ["grp-any"]
  direction       = { lan_to_lan = false, lan_to_wan = true }
  depends_on      = [omada_firewall_acl.permit_a]
}
`

// TestAccFirewallACLOrderResource drives create → import → update of
// omada_firewall_acl_order against the in-test mock. Requires TF_ACC=1.
func TestAccFirewallACLOrderResource(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{ // create: put both permits above the deny they were created after
				Config: testProviderConfig(srv.URL) + aclOrderRules + `
resource "omada_firewall_acl_order" "gateway" {
  rule_ids = [
    omada_firewall_acl.permit_a.id,
    omada_firewall_acl.permit_b.id,
    omada_firewall_acl.deny_wan.id,
  ]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_firewall_acl_order.gateway", "type", "gateway"),
					resource.TestCheckResourceAttr("omada_firewall_acl_order.gateway", "site_id", "site-1"),
					// The canonical site name is recorded even when unset (§2.7a).
					resource.TestCheckResourceAttr("omada_firewall_acl_order.gateway", "site", "Default"),
					resource.TestCheckResourceAttr("omada_firewall_acl_order.gateway", "id", "site-1/gateway"),
					resource.TestCheckResourceAttr("omada_firewall_acl_order.gateway", "rule_ids.#", "3"),
					// The order read back from the controller, not just echoed
					// from config: permits first, deny last.
					resource.TestCheckResourceAttrPair(
						"omada_firewall_acl_order.gateway", "rule_ids.0",
						"omada_firewall_acl.permit_a", "id"),
					resource.TestCheckResourceAttrPair(
						"omada_firewall_acl_order.gateway", "rule_ids.1",
						"omada_firewall_acl.permit_b", "id"),
					resource.TestCheckResourceAttrPair(
						"omada_firewall_acl_order.gateway", "rule_ids.2",
						"omada_firewall_acl.deny_wan", "id"),
				),
			},
			{ // import by type alone — the order is a per-site/type singleton.
				// rule_ids cannot be discovered (which rules the config intends
				// to order is not knowable from the controller), so it is
				// ignored on verify, as with the other sparse resources.
				ResourceName:            "omada_firewall_acl_order.gateway",
				ImportState:             true,
				ImportStateId:           "gateway",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"rule_ids"},
			},
			{ // update: swap the two permits
				Config: testProviderConfig(srv.URL) + aclOrderRules + `
resource "omada_firewall_acl_order" "gateway" {
  rule_ids = [
    omada_firewall_acl.permit_b.id,
    omada_firewall_acl.permit_a.id,
    omada_firewall_acl.deny_wan.id,
  ]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrPair(
						"omada_firewall_acl_order.gateway", "rule_ids.0",
						"omada_firewall_acl.permit_b", "id"),
					resource.TestCheckResourceAttrPair(
						"omada_firewall_acl_order.gateway", "rule_ids.1",
						"omada_firewall_acl.permit_a", "id"),
					resource.TestCheckResourceAttrPair(
						"omada_firewall_acl_order.gateway", "rule_ids.2",
						"omada_firewall_acl.deny_wan", "id"),
				),
			},
		},
	})
}

// TestAccFirewallACLOrderResource_UnknownRule covers the client's own guard: an
// id that is not on the controller must fail with a clear message rather than
// being sent and coming back as the controller's opaque
// -1001 "Invalid request parameters.".
func TestAccFirewallACLOrderResource_UnknownRule(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_firewall_acl_order" "gateway" {
  rule_ids = ["acl-seed", "acl-does-not-exist"]
}`,
				ExpectError: regexp.MustCompile(`acl "acl-does-not-exist" not found`),
			},
		},
	})
}

// TestAccFirewallACLOrderImportWithoutSitePrefix pins DESIGN.md §2.7a for this
// resource: a bare import id must not leave site null, or a configuration that
// names its site would plan a replacement of what it just imported.
func TestAccFirewallACLOrderImportWithoutSitePrefix(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_firewall_acl_order" "gateway" {
  site     = "Default"
  rule_ids = ["acl-seed"]
}`,
			},
			{
				ResourceName: "omada_firewall_acl_order.gateway",
				ImportState:  true,
				// Deliberately bare: no "<site>/" prefix.
				ImportStateId:           "gateway",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"rule_ids"},
			},
		},
	})
}

// TestAccFirewallACLOrderResource_UnverifiedType keeps switch and EAP out until
// modifyIndex has been exercised for them on a live controller.
func TestAccFirewallACLOrderResource_UnverifiedType(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_firewall_acl_order" "switch" {
  type     = "switch"
  rule_ids = ["acl-seed"]
}`,
				ExpectError: regexp.MustCompile(`(?s)value must be one of:.*"gateway"`),
			},
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_firewall_acl_order" "switch" {
  rule_ids = ["acl-seed"]
}`,
				ResourceName:  "omada_firewall_acl_order.switch",
				ImportState:   true,
				ImportStateId: "switch",
				ExpectError:   regexp.MustCompile(`Invalid import ID`),
			},
		},
	})
}
