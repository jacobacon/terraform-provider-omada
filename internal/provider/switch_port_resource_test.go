// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccSwitchPortResource drives adopt → import → update of omada_switch_port.
//
// The behaviour worth protecting is partial management: the first step sets
// only `name`, and the port's VLAN configuration must survive untouched. A
// provider that sent a plan-shaped body instead of a merged one would blank
// those fields, and the checks on nativeNetworkId would catch it.
func TestAccSwitchPortResource(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfigOpenAPI(srv.URL) + `
resource "omada_switch_port" "nas" {
  switch_mac = "8c:86:dd:10:50:ca"
  port       = 4
  name       = "NAS"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_switch_port.nas", "id", "8C-86-DD-10-50-CA/4"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "name", "NAS"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "site_id", "site-1"),
					// Untouched by this step, so still the seeded values.
					resource.TestCheckResourceAttr("omada_switch_port.nas", "native_network_id", "net-1"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "network_tags_setting", "2"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "profile_id", "prof-all"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "profile_name", "All"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "duplex", "0"),
					// The tag's label is surfaced, so a plan shows "AP" rather
					// than only an opaque id.
					resource.TestCheckResourceAttr("omada_switch_port.nas", "tag_name", "AP"),
				),
			},
			{
				ResourceName:      "omada_switch_port.nas",
				ImportState:       true,
				ImportStateId:     "8C-86-DD-10-50-CA/4",
				ImportStateVerify: true,
				// The config spells the MAC with colons and the controller with
				// dashes. The provider treats those as the same value (macType),
				// but ImportStateVerify compares state strings textually and has
				// no way to know that, so it must be told to skip this one. That
				// the equality really works is what the next step shows: it
				// keeps the colon spelling and plans an update, not a
				// replacement, even though switch_mac forces replacement.
				ImportStateVerifyIgnore: []string{"switch_mac"},
			},
			{
				Config: testProviderConfigOpenAPI(srv.URL) + `
resource "omada_switch_port" "nas" {
  switch_mac = "8c:86:dd:10:50:ca"
  port       = 4
  name       = "NAS (10G)"

  profile_id                   = "prof-main"
  profile_vlan_override_enable = true
  native_network_id            = "net-2"
  network_tags_setting         = 1
  tag_ids                      = ["net-1"]
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("omada_switch_port.nas", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_switch_port.nas", "name", "NAS (10G)"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "profile_id", "prof-main"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "profile_name", "MAIN"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "native_network_id", "net-2"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "network_tags_setting", "1"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "tag_ids.#", "1"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "tag_ids.0", "net-1"),
				),
			},
		},
	})
}

// TestAccSwitchPortRequiresOpenAPI checks the failure a practitioner meets when
// they configure a port with only the admin credentials.
//
// This is the whole reason ErrOpenAPINotConfigured spells out where the
// credentials come from: without it the controller's own answer is -44116
// "Open API Authorized failed", which reads like the username and password are
// wrong and sends people to change the one thing that was already correct.
func TestAccSwitchPortRequiresOpenAPI(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_switch_port" "nas" {
  switch_mac = "8C-86-DD-10-50-CA"
  port       = 4
  name       = "NAS"
}`,
				ExpectError: regexp.MustCompile(`Platform Integration -> Open API`),
			},
		},
	})
}

// TestAccSwitchPortWebAPIWrite configures a port with only the admin
// credentials on a controller that has the v5.15 web-API port write. The mock
// refuses any body that is not the whole port document, and the check on poe
// shows the unmodelled keys were sent back as they were read.
func TestAccSwitchPortWebAPIWrite(t *testing.T) {
	srv := newMockController(t, withV5Controller())

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_switch_port" "nas" {
  switch_mac = "8C-86-DD-10-50-CA"
  port       = 4
  name       = "NAS"
  profile_id = "prof-main"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_switch_port.nas", "name", "NAS"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "profile_id", "prof-main"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "profile_name", "MAIN"),
					resource.TestCheckResourceAttr("omada_switch_port.nas", "native_network_id", "net-1"),
					func(*terraform.State) error {
						port := rawStore(t, srv.URL, "switchPorts")["4"]
						if port["poe"] != float64(1) || port["operation"] != "switching" {
							return fmt.Errorf("unmodelled keys changed by the write: poe=%v operation=%v",
								port["poe"], port["operation"])
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccSwitchPortWebAPIUnknownProfile sets a profile id the controller does
// not have. v5.15 answers that with success and keeps the old profile, so the
// provider has to catch it on the read-back.
func TestAccSwitchPortWebAPIUnknownProfile(t *testing.T) {
	srv := newMockController(t, withV5Controller())

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_switch_port" "nas" {
  switch_mac = "8C-86-DD-10-50-CA"
  port       = 4
  name       = "NAS"
  profile_id = "prof-missing"
}`,
				ExpectError: regexp.MustCompile(`kept its old profile on port 4`),
			},
		},
	})
}
