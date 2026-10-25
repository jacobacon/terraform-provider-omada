// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccNetworksDataSource runs the omada_networks data source against the
// in-test mock controller (default site resolution + one network). Requires
// TF_ACC=1; no real controller needed, so it runs in CI.
func TestAccNetworksDataSource(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `data "omada_networks" "test" {}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.omada_networks.test", "site_id", "site-1"),
					resource.TestCheckResourceAttr("data.omada_networks.test", "networks.#", "1"),
					resource.TestCheckResourceAttr("data.omada_networks.test", "networks.0.id", "net-1"),
					resource.TestCheckResourceAttr("data.omada_networks.test", "networks.0.name", "IoT"),
					resource.TestCheckResourceAttr("data.omada_networks.test", "networks.0.vlan_id", "30"),
					resource.TestCheckResourceAttr("data.omada_networks.test", "networks.0.gateway_subnet", "192.168.30.1/24"),
					resource.TestCheckResourceAttr("data.omada_networks.test", "networks.0.dhcp_enabled", "true"),
					resource.TestCheckResourceAttr("data.omada_networks.test", "networks.0.builtin", "false"),
				),
			},
		},
	})
}
