// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccPortProfilesDataSource creates a VLAN network and reads back the port
// profile the controller makes for it: same name, origin "network", and the
// new VLAN as its native network.
func TestAccPortProfilesDataSource(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_network" "cameras" {
  name    = "cameras"
  purpose = "vlan"
  vlan_id = 40
}

data "omada_port_profiles" "all" {
  depends_on = [omada_network.cameras]
}

output "cameras_profile_native" {
  value = [for p in data.omada_port_profiles.all.profiles : p.native_network_id if p.name == "cameras"][0]
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.omada_port_profiles.all", "site_id", "site-1"),
					resource.TestCheckTypeSetElemNestedAttrs("data.omada_port_profiles.all", "profiles.*", map[string]string{
						"name": "cameras", "origin": "network",
					}),
					func(st *terraform.State) error {
						net := st.RootModule().Resources["omada_network.cameras"].Primary.ID
						out := st.RootModule().Outputs["cameras_profile_native"]
						if out == nil || out.Value != net {
							return fmt.Errorf("profile's native network is %v, want %s", out, net)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccPortProfileNameCollision checks that the controller's refusal to
// create a second profile under a VLAN network's name reaches the user.
func TestAccPortProfileNameCollision(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_network" "cameras" {
  name    = "cameras"
  purpose = "vlan"
  vlan_id = 40
}

resource "omada_port_profile" "cameras" {
  name       = "cameras"
  depends_on = [omada_network.cameras]
}`,
				ExpectError: regexp.MustCompile(`already\s+exists`),
			},
		},
	})
}
