// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/wncservices/terraform-provider-omada/internal/omada"
)

var _ datasource.DataSource = &firewallACLsDataSource{}
var _ datasource.DataSourceWithConfigure = &firewallACLsDataSource{}

func NewFirewallACLsDataSource() datasource.DataSource { return &firewallACLsDataSource{} }

type firewallACLsDataSource struct {
	data *providerData
}

type firewallACLItemModel struct {
	ID      types.String `tfsdk:"id"`
	Type    types.String `tfsdk:"type"`
	Name    types.String `tfsdk:"name"`
	Enabled types.Bool   `tfsdk:"enabled"`
	Policy  types.Int64  `tfsdk:"policy"`
	Index   types.Int64  `tfsdk:"index"`
}

type firewallACLsDataSourceModel struct {
	SiteID   types.String           `tfsdk:"site_id"`
	SiteName types.String           `tfsdk:"site"`
	ACLs     []firewallACLItemModel `tfsdk:"acls"`
}

// aclTypesToList are the ACL types this data source enumerates, with their
// human-readable names (matching the omada_firewall_acl resource's `type`).
var aclTypesToList = []struct {
	code int
	name string
}{
	{omada.ACLTypeGateway, "gateway"},
	{omada.ACLTypeSwitch, "switch"},
	{omada.ACLTypeEAP, "eap"},
}

func (d *firewallACLsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_firewall_acls"
}

func (d *firewallACLsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists firewall ACL rules (across gateway, switch and EAP types) for a site — handy for discovering rule IDs to import.",
		Attributes: map[string]schema.Attribute{
			"site_id": schema.StringAttribute{
				MarkdownDescription: "Site ID to query. Provide this or `site`; if neither is set, the provider's default site is used.",
				Optional:            true,
				Computed:            true,
			},
			"site": schema.StringAttribute{
				MarkdownDescription: "Site name to query (resolved to an ID). Ignored if `site_id` is set.",
				Optional:            true,
			},
			"acls": schema.ListNestedAttribute{
				MarkdownDescription: "The firewall ACL rules defined on the site.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":      schema.StringAttribute{Computed: true, MarkdownDescription: "The rule ID (use this to import `omada_firewall_acl`)."},
						"type":    schema.StringAttribute{Computed: true, MarkdownDescription: "ACL type: `gateway`, `switch`, or `eap`."},
						"name":    schema.StringAttribute{Computed: true, MarkdownDescription: "The rule name."},
						"enabled": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the rule is enabled."},
						"policy":  schema.Int64Attribute{Computed: true, MarkdownDescription: "Policy: 0=deny, 1=permit."},
						"index": schema.Int64Attribute{Computed: true, MarkdownDescription: "1-based position in this type's evaluation order. " +
							"Omada evaluates top to bottom and the first match wins, so this is what decides which of two overlapping rules applies. " +
							"Set it with `omada_firewall_acl_order`; it cannot be set on the rule itself."},
					},
				},
			},
		},
	}
}

func (d *firewallACLsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *providerData, got %T", req.ProviderData))
		return
	}
	d.data = data
}

func (d *firewallACLsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config firewallACLsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	siteID, diags := d.resolveSiteID(ctx, config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state := firewallACLsDataSourceModel{
		SiteID:   types.StringValue(siteID),
		SiteName: config.SiteName,
	}
	for _, t := range aclTypesToList {
		acls, err := d.data.client.ListACLs(ctx, siteID, t.code)
		if err != nil {
			resp.Diagnostics.AddError(fmt.Sprintf("Unable to list %s ACLs", t.name), err.Error())
			return
		}
		// Listed in evaluation order, so reading the list IS reading the order.
		sort.SliceStable(acls, func(i, j int) bool { return acls[i].Index < acls[j].Index })
		for _, a := range acls {
			state.ACLs = append(state.ACLs, firewallACLItemModel{
				ID:      types.StringValue(a.ID),
				Type:    types.StringValue(t.name),
				Name:    types.StringValue(a.Name),
				Enabled: types.BoolValue(a.Status),
				Policy:  types.Int64Value(int64(a.Policy)),
				Index:   types.Int64Value(int64(a.Index)),
			})
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (d *firewallACLsDataSource) resolveSiteID(ctx context.Context, config firewallACLsDataSourceModel) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if !config.SiteID.IsNull() && config.SiteID.ValueString() != "" {
		return config.SiteID.ValueString(), diags
	}
	name := d.data.defaultSite
	if !config.SiteName.IsNull() && config.SiteName.ValueString() != "" {
		name = config.SiteName.ValueString()
	}
	id, err := d.data.client.ResolveSiteID(ctx, name)
	if err != nil {
		diags.AddError("Unable to resolve site", err.Error())
		return "", diags
	}
	return id, diags
}
