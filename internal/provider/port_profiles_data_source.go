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

var _ datasource.DataSource = &portProfilesDataSource{}
var _ datasource.DataSourceWithConfigure = &portProfilesDataSource{}

// profileOrigin names the controller's `flag` on a port profile. Anything
// other than the three observed values is "unknown", which a configuration
// that deletes profiles should treat as not its own.
func profileOrigin(flag int) string {
	switch flag {
	case 0:
		return "builtin"
	case 1:
		return "network"
	case 2:
		return "user"
	default:
		return "unknown"
	}
}

// NewPortProfilesDataSource lists switch port profiles. It resolves a profile
// name to the id that `omada_switch_port.profile_id` takes, for profiles
// Terraform does not manage (the built-ins, or one the controller made for a
// VLAN network), and reports who owns each one.
func NewPortProfilesDataSource() datasource.DataSource {
	return &portProfilesDataSource{}
}

type portProfilesDataSource struct {
	data *providerData
}

type portProfileSummary struct {
	ID              types.String `tfsdk:"id"`
	Name            types.String `tfsdk:"name"`
	NativeNetworkID types.String `tfsdk:"native_network_id"`
	Origin          types.String `tfsdk:"origin"`
}

type portProfilesDataSourceModel struct {
	SiteID   types.String         `tfsdk:"site_id"`
	SiteName types.String         `tfsdk:"site"`
	Profiles []portProfileSummary `tfsdk:"profiles"`
}

func (d *portProfilesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_port_profiles"
}

func (d *portProfilesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists switch port profiles for a site on the Omada controller.",
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
			"profiles": schema.ListNestedAttribute{
				MarkdownDescription: "The port profiles defined on the site.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":                schema.StringAttribute{Computed: true, MarkdownDescription: "The profile ID."},
						"name":              schema.StringAttribute{Computed: true, MarkdownDescription: "The profile name."},
						"native_network_id": schema.StringAttribute{Computed: true, MarkdownDescription: "The untagged (native) network ID, empty for a profile with none."},
						"origin": schema.StringAttribute{Computed: true, MarkdownDescription: "Who owns the profile: " +
							"`builtin` (All, Default and Disable, which cannot be deleted), `network` (created by the " +
							"controller along with a VLAN network, carrying it untagged, and deleted with it), " +
							"`user` (created in the UI or through this provider), or `unknown`."},
					},
				},
			},
		},
	}
}

func (d *portProfilesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *portProfilesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config portProfilesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	siteID, diags := d.resolveSiteID(ctx, config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	profiles, err := d.data.client.ListPortProfiles(ctx, siteID)
	if err != nil {
		resp.Diagnostics.AddError("Unable to list Omada port profiles", err.Error())
		return
	}

	state := portProfilesDataSourceModel{
		SiteID:   types.StringValue(siteID),
		SiteName: config.SiteName,
		Profiles: []portProfileSummary{},
	}
	for _, p := range profiles {
		state.Profiles = append(state.Profiles, portProfileSummary{
			ID:              types.StringValue(p.ID),
			Name:            types.StringValue(p.Name),
			NativeNetworkID: types.StringValue(p.NativeNetworkID),
			Origin:          types.StringValue(profileOrigin(p.Flag)),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// resolveSiteID picks the site to query: explicit site_id, else site name,
// else the provider's default site.
func (d *portProfilesDataSource) resolveSiteID(ctx context.Context, config portProfilesDataSourceModel) (string, diag.Diagnostics) {
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
