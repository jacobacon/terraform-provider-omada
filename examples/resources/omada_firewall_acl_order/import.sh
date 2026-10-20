# The order is a per-site, per-type singleton, so the ACL type is the whole ID.
terraform import omada_firewall_acl_order.gateway gateway

# On a non-default site, prefix the site name:
terraform import omada_firewall_acl_order.gateway 'Branch Office/gateway'

# The import brings in no rule_ids -- which rules a configuration intends to
# order is not discoverable from the controller -- so expect a one-time diff
# that adds them.
