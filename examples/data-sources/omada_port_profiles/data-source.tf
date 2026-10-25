# Uses the provider's default site unless site or site_id is given.
data "omada_port_profiles" "all" {}

# Look up a profile the controller created for a VLAN network, by name.
locals {
  iot_profile_id = [for p in data.omada_port_profiles.all.profiles : p.id if p.name == "IoT"][0]
}
