// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// TestAccSwitchResource drives adopt → rename → import of omada_switch, and
// checks that destroy leaves the last applied name on the controller.
func TestAccSwitchResource(t *testing.T) {
	srv := newMockController(t)
	const mac = "8C-86-DD-10-50-CA"
	controllerName := func() string {
		name, _ := rawStore(t, srv.URL, "devices")[mac]["name"].(string)
		return name
	}
	expectName := func(want string) resource.TestCheckFunc {
		return func(*terraform.State) error {
			if got := controllerName(); got != want {
				return fmt.Errorf("controller has name %q, want %q", got, want)
			}
			return nil
		}
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			return expectName("Closet Switch")(nil)
		},
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_switch" "sw" {
  mac  = "8c:86:dd:10:50:ca"
  name = "Lab Switch"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_switch.sw", "id", mac),
					resource.TestCheckResourceAttr("omada_switch.sw", "name", "Lab Switch"),
					resource.TestCheckResourceAttr("omada_switch.sw", "model", "ES205GP"),
					resource.TestCheckResourceAttr("omada_switch.sw", "site_id", "site-1"),
					expectName("Lab Switch"),
				),
			},
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_switch" "sw" {
  mac  = "8c:86:dd:10:50:ca"
  name = "Closet Switch"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_switch.sw", "name", "Closet Switch"),
					expectName("Closet Switch"),
				),
			},
			{
				ResourceName:      "omada_switch.sw",
				ImportState:       true,
				ImportStateId:     mac,
				ImportStateVerify: true,
				// The config spells the MAC with colons and the controller with
				// dashes; see TestAccSwitchPortResource.
				ImportStateVerifyIgnore: []string{"mac"},
			},
		},
	})
}

// TestAccSwitchResourceNotASwitch refuses a MAC that belongs to another kind
// of device, which has its own resource and document path.
func TestAccSwitchResourceNotASwitch(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_switch" "ap" {
  mac  = "60-83-E7-4B-1B-40"
  name = "Hallway AP"
}`,
				ExpectError: regexp.MustCompile(`not a switch`),
			},
		},
	})
}

// TestAccSwitchResourceReadErrors keeps the switch in state when the device
// list fails, and only drops it once the switch has left the site.
func TestAccSwitchResourceReadErrors(t *testing.T) {
	srv := newMockController(t)
	debug := func(method, query string) func() {
		return func() {
			req, _ := http.NewRequest(method, srv.URL+"/debug/devices?"+query, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("debug %s: %v", method, err)
			}
			_ = resp.Body.Close()
		}
	}
	config := testProviderConfig(srv.URL) + `
resource "omada_switch" "sw" {
  mac  = "8C-86-DD-10-50-CA"
  name = "Lab Switch"
}`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig:   debug(http.MethodPost, "down=true"),
				Config:      config,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Unable to read switch`),
			},
			{
				PreConfig: func() {
					debug(http.MethodPost, "down=false")()
					debug(http.MethodDelete, "mac=8C-86-DD-10-50-CA")()
				},
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}
