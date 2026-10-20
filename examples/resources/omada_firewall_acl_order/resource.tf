# Omada evaluates gateway ACLs top to bottom and the first match wins, and a
# newly created rule always APPENDS to the end of the list. So a deny created
# before the permits that should override it ends up above them, silently
# shadowing both — the rules all exist, look correct, and do the wrong thing.
#
# This resource asserts the evaluation order.

resource "omada_firewall_acl" "permit_frigate_wan" {
  name            = "Permit Frigate to WAN"
  policy          = "permit"
  source_type     = 1 # IP group
  source_ids      = [var.frigate_group_id]
  destination_ids = [var.any_group_id]
  direction       = { lan_to_lan = false, lan_to_wan = true }
}

resource "omada_firewall_acl" "permit_nvr_wan" {
  name            = "Permit NVR to WAN"
  policy          = "permit"
  source_type     = 1
  source_ids      = [var.nvr_group_id]
  destination_ids = [var.any_group_id]
  direction       = { lan_to_lan = false, lan_to_wan = true }
}

resource "omada_firewall_acl" "deny_cameras_wan" {
  name            = "Deny Cameras to WAN"
  policy          = "deny"
  source_type     = 0 # LAN network — the whole camera VLAN
  source_ids      = [var.cameras_network_id]
  destination_ids = [var.any_group_id]
  direction       = { lan_to_lan = false, lan_to_wan = true }
}

# The two permits must evaluate BEFORE the deny, or the cameras VLAN loses its
# exceptions and Frigate stops getting image pulls while detection carries on
# working — a failure that looks like nothing happened.
resource "omada_firewall_acl_order" "gateway" {
  rule_ids = [
    omada_firewall_acl.permit_frigate_wan.id,
    omada_firewall_acl.permit_nvr_wan.id,
    omada_firewall_acl.deny_cameras_wan.id,
  ]
}

# Reading the current order: the data source lists each type in evaluation
# order and exposes `index`.
data "omada_firewall_acls" "all" {}

output "gateway_acl_order" {
  value = [
    for a in data.omada_firewall_acls.all.acls : "${a.index}: ${a.name}"
    if a.type == "gateway"
  ]
}
