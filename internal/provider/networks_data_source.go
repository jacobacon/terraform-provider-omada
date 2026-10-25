// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = &networksDataSource{}
var _ datasource.DataSourceWithConfigure = &networksDataSource{}

func NewNetworksDataSource() datasource.DataSource {
	return &networksDataSource{}
}

type networksDataSource struct {
	data *providerData
}

type networkModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Purpose       types.String `tfsdk:"purpose"`
	VLANID        types.Int64  `tfsdk:"vlan_id"`
	GatewaySubnet types.String `tfsdk:"gateway_subnet"`
	DHCPEnabled   types.Bool   `tfsdk:"dhcp_enabled"`
	Builtin       types.Bool   `tfsdk:"builtin"`
}

type networksDataSourceModel struct {
	SiteID   types.String   `tfsdk:"site_id"`
	SiteName types.String   `tfsdk:"site"`
	Networks []networkModel `tfsdk:"networks"`
}

func (d *networksDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_networks"
}

func (d *networksDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists LAN networks (VLANs) for a site on the Omada controller.",
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
			"networks": schema.ListNestedAttribute{
				MarkdownDescription: "The LAN networks defined on the site.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":             schema.StringAttribute{Computed: true, MarkdownDescription: "The network ID."},
						"name":           schema.StringAttribute{Computed: true, MarkdownDescription: "The network name."},
						"purpose":        schema.StringAttribute{Computed: true, MarkdownDescription: "The network purpose (`interface` or `vlan`)."},
						"vlan_id":        schema.Int64Attribute{Computed: true, MarkdownDescription: "The VLAN ID."},
						"gateway_subnet": schema.StringAttribute{Computed: true, MarkdownDescription: "Gateway IP + subnet in CIDR notation."},
						"dhcp_enabled":   schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether DHCP is enabled on the network."},
						"builtin":        schema.BoolAttribute{Computed: true, MarkdownDescription: "True for the controller's primary (Default) network, which cannot be deleted."},
					},
				},
			},
		},
	}
}

func (d *networksDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *networksDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config networksDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	siteID, diags := d.resolveSiteID(ctx, config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	networks, err := d.data.client.ListNetworks(ctx, siteID)
	if err != nil {
		resp.Diagnostics.AddError("Unable to list Omada networks", err.Error())
		return
	}

	state := networksDataSourceModel{
		SiteID:   types.StringValue(siteID),
		SiteName: config.SiteName,
	}
	for _, n := range networks {
		state.Networks = append(state.Networks, networkModel{
			ID:            types.StringValue(n.ID),
			Name:          types.StringValue(n.Name),
			Purpose:       types.StringValue(n.Purpose),
			VLANID:        types.Int64Value(int64(n.VLANID)),
			GatewaySubnet: types.StringValue(n.GatewaySubnet),
			DHCPEnabled:   types.BoolValue(n.DHCPEnabled()),
			Builtin:       types.BoolValue(n.Primary),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// resolveSiteID picks the site to query: explicit site_id, else site name,
// else the provider's default site.
func (d *networksDataSource) resolveSiteID(ctx context.Context, config networksDataSourceModel) (string, diag.Diagnostics) {
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
