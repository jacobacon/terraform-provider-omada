// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/wncservices/terraform-provider-omada/internal/omada"
)

var (
	_ resource.Resource                = &switchPortResource{}
	_ resource.ResourceWithConfigure   = &switchPortResource{}
	_ resource.ResourceWithImportState = &switchPortResource{}
)

func NewSwitchPortResource() resource.Resource { return &switchPortResource{} }

type switchPortResource struct{ data *providerData }

type switchPortResourceModel struct {
	ID        types.String `tfsdk:"id"`
	Site      types.String `tfsdk:"site"`
	SiteID    types.String `tfsdk:"site_id"`
	SwitchMAC macValue     `tfsdk:"switch_mac"`
	Port      types.Int64  `tfsdk:"port"`

	Name                      types.String `tfsdk:"name"`
	ProfileID                 types.String `tfsdk:"profile_id"`
	ProfileName               types.String `tfsdk:"profile_name"`
	ProfileOverrideEnable     types.Bool   `tfsdk:"profile_override_enable"`
	ProfileVLANOverrideEnable types.Bool   `tfsdk:"profile_vlan_override_enable"`
	NativeNetworkID           types.String `tfsdk:"native_network_id"`
	NetworkTagsSetting        types.Int64  `tfsdk:"network_tags_setting"`
	TagIDs                    types.List   `tfsdk:"tag_ids"`
	TagName                   types.String `tfsdk:"tag_name"`
	Duplex                    types.Int64  `tfsdk:"duplex"`
	LinkSpeed                 types.Int64  `tfsdk:"link_speed"`
}

func (r *switchPortResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_switch_port"
}

func (r *switchPortResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the configuration of one physical port on an adopted switch.\n\n" +
			"~> **Writes prefer Open API credentials** (`openapi_client_id` / " +
			"`openapi_client_secret`). The read always comes from the web API. On v6.2 controllers the " +
			"web API has no per-port write, so the admin username and password alone are not enough and " +
			"the failure is at apply time, not plan. v5 controllers do have one, and without Open API " +
			"credentials the provider uses it instead. That route only accepts the whole port document, " +
			"so the write is a read-modify-write and can overwrite a UI change made at the same moment.\n\n" +
			"~> **A port is physical: it cannot be created or destroyed.** Terraform *adopts* an " +
			"existing port here. Removing the resource from your configuration leaves the port exactly " +
			"as it was last applied — Terraform forgets it, the switch does not. Reconfiguring the port " +
			"a device is plugged into will interrupt that device's traffic, and reconfiguring an uplink " +
			"can cut off everything behind it, including the switch itself.\n\n" +
			"Every settable attribute is optional: whatever you leave out keeps its current value on " +
			"the controller rather than being reset to a default. That makes it safe to manage just the " +
			"name, or just the VLAN, on a port whose other settings you would rather not touch — but it " +
			"also means an omitted attribute will never report drift.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`<switch mac>/<port>`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"site": schema.StringAttribute{
				Optional: true,
				// Computed as well as Optional, and recorded as the *canonical*
				// resolved name. Optional-only looks simpler but sets a trap:
				// importing without a "<site>/" prefix leaves this null, so a
				// configuration that names the site reads as a change — and
				// since the attribute forces replacement, the plan proposes
				// replacing hardware it had just adopted.
				Computed:            true,
				MarkdownDescription: "Site name. Defaults to the primary site. Changing forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace(),
				},
			},
			"site_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"switch_mac": schema.StringAttribute{
				CustomType: macType{},
				Required:   true,
				MarkdownDescription: "MAC address of the switch, in any common spelling " +
					"(`8C-86-DD-10-50-CA`, `8c:86:dd:10:50:ca`). See the `omada_devices` data source.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"port": schema.Int64Attribute{
				Required:            true,
				MarkdownDescription: "Physical port number, starting at 1.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},

			"name": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Port label shown in the controller, e.g. `Port4` or `NAS`.",
			},
			"profile_id": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "ID of the port profile applied to this port (see `omada_port_profile`). " +
					"The profile is what normally decides the port's VLAN membership.",
			},
			"profile_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Name of the applied profile. Read-only, for readability in plans and state.",
			},
			"profile_override_enable": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Whether per-port settings override the profile.",
			},
			"profile_vlan_override_enable": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Whether the per-port VLAN settings below override the profile's.",
			},
			"native_network_id": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "ID of the untagged (native/PVID) network on this port — the VLAN an " +
					"unaware device plugged in here lands on. See `omada_networks`.\n\n" +
					"~> Changing this moves whatever is plugged into the port onto a different VLAN, and " +
					"on an uplink it will break the trunk.",
			},
			"network_tags_setting": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Controller enum for how tagged networks are selected on this port. " +
					"Observed live: `1` on access ports carrying a single network, `2` on ports set to " +
					"carry all of them. The controller does not publish the full enum.",
			},
			"tag_ids": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true, Computed: true,
				MarkdownDescription: "IDs of the **port tags** applied to this port. Despite the name " +
					"these are not network ids — see `tag_name` for what the controller calls them.\n\n" +
					"~> A tag deleted from the site is not cleared from the ports that reference it. The " +
					"id simply stays, and the controller carries on reporting its old name, so a value " +
					"here can outlive the tag itself.",
			},
			"tag_name": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The controller's label for the applied port tag, e.g. `AP`. " +
					"Read-only, and present so `tag_ids` is legible rather than opaque.",
			},
			"duplex": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Configured duplex, where `0` is auto-negotiate. This is the setting, " +
					"not the negotiated link state.\n\n" +
					"~> Forcing duplex or speed on a port whose peer auto-negotiates is a classic way to " +
					"produce a duplex mismatch: the link stays up and quietly loses packets under load. " +
					"Leave it at auto unless you are fixing a known-bad link.",
			},
			"link_speed": schema.Int64Attribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Configured link speed, where `0` is auto-negotiate. See the warning on `duplex`.",
			},
		},
	}
}

func (r *switchPortResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.data = data
}

func (r *switchPortResource) siteName(m switchPortResourceModel) string {
	if !m.Site.IsNull() && m.Site.ValueString() != "" {
		return m.Site.ValueString()
	}
	return r.data.defaultSite
}

func (r *switchPortResource) resolveSite(ctx context.Context, m switchPortResourceModel, diags *diagSink) (string, string) {
	site, err := r.data.client.ResolveSite(ctx, r.siteName(m))
	if err != nil {
		diags.AddError("Unable to resolve site", err.Error())
		return "", ""
	}
	return site.ID, site.Name
}

// merge overlays the configured attributes of the plan onto the port as it
// currently exists, leaving everything the practitioner did not set alone.
//
// This read-modify-write is what makes partial management safe: the PATCH body
// always carries a complete, current set of values, so managing only `name`
// cannot silently blank the port's VLAN configuration.
func (r *switchPortResource) merge(ctx context.Context, cur omada.SwitchPort, plan switchPortResourceModel, diags *diagSink) omada.SwitchPort {
	out := cur
	if !plan.Name.IsUnknown() && !plan.Name.IsNull() {
		out.Name = plan.Name.ValueString()
	}
	if !plan.ProfileID.IsUnknown() && !plan.ProfileID.IsNull() {
		out.ProfileID = plan.ProfileID.ValueString()
	}
	if !plan.ProfileOverrideEnable.IsUnknown() && !plan.ProfileOverrideEnable.IsNull() {
		out.ProfileOverrideEnable = plan.ProfileOverrideEnable.ValueBool()
	}
	if !plan.ProfileVLANOverrideEnable.IsUnknown() && !plan.ProfileVLANOverrideEnable.IsNull() {
		out.ProfileVLANOverrideEnable = plan.ProfileVLANOverrideEnable.ValueBool()
	}
	if !plan.NativeNetworkID.IsUnknown() && !plan.NativeNetworkID.IsNull() {
		out.NativeNetworkID = plan.NativeNetworkID.ValueString()
	}
	if !plan.NetworkTagsSetting.IsUnknown() && !plan.NetworkTagsSetting.IsNull() {
		out.NetworkTagsSetting = int(plan.NetworkTagsSetting.ValueInt64())
	}
	if !plan.TagIDs.IsUnknown() && !plan.TagIDs.IsNull() {
		tags, d := stringSlice(ctx, plan.TagIDs)
		if d.HasError() {
			diags.AddError("Invalid tag_ids", "could not read the tag id list")
		}
		out.TagIDs = tags
	}
	if !plan.Duplex.IsUnknown() && !plan.Duplex.IsNull() {
		out.Duplex = int(plan.Duplex.ValueInt64())
	}
	if !plan.LinkSpeed.IsUnknown() && !plan.LinkSpeed.IsNull() {
		out.LinkSpeed = int(plan.LinkSpeed.ValueInt64())
	}
	return out
}

func (r *switchPortResource) refresh(ctx context.Context, p *omada.SwitchPort, m *switchPortResourceModel) {
	m.ID = types.StringValue(fmt.Sprintf("%s/%d", omada.NormalizeMAC(m.SwitchMAC.ValueString()), p.Port))
	m.Port = types.Int64Value(int64(p.Port))
	m.Name = types.StringValue(p.Name)
	m.ProfileID = types.StringValue(p.ProfileID)
	m.ProfileName = types.StringValue(p.ProfileName)
	m.ProfileOverrideEnable = types.BoolValue(p.ProfileOverrideEnable)
	m.ProfileVLANOverrideEnable = types.BoolValue(p.ProfileVLANOverrideEnable)
	m.NativeNetworkID = types.StringValue(p.NativeNetworkID)
	m.NetworkTagsSetting = types.Int64Value(int64(p.NetworkTagsSetting))
	m.TagName = types.StringValue(p.TagName)
	m.Duplex = types.Int64Value(int64(p.Duplex))
	m.LinkSpeed = types.Int64Value(int64(p.LinkSpeed))
	lv, _ := stringListValue(ctx, p.TagIDs)
	m.TagIDs = lv
}

// apply is the shared body of Create and Update: read the port, overlay the
// plan, write, then read back what the controller actually stored.
func (r *switchPortResource) apply(ctx context.Context, plan *switchPortResourceModel, diags *diagSink) {
	siteID, siteName := r.resolveSite(ctx, *plan, diags)
	if siteID == "" {
		return
	}
	mac := plan.SwitchMAC.ValueString()
	port := int(plan.Port.ValueInt64())

	cur, err := r.data.client.GetSwitchPort(ctx, siteID, mac, port)
	if err != nil {
		diags.AddError("Unable to read switch port", err.Error())
		return
	}

	want := r.merge(ctx, *cur, *plan, diags)
	if err := r.data.client.UpdateSwitchPort(ctx, siteID, mac, port, want); err != nil {
		diags.AddError("Unable to update switch port", err.Error())
		return
	}

	after, err := r.data.client.GetSwitchPort(ctx, siteID, mac, port)
	if err != nil {
		diags.AddError("Unable to read switch port after update", err.Error())
		return
	}
	r.refresh(ctx, after, plan)
	plan.SiteID = types.StringValue(siteID)
	plan.Site = types.StringValue(siteName)
}

// Create adopts an existing port rather than making one.
func (r *switchPortResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan switchPortResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, &plan, &diagSink{&resp.Diagnostics})
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *switchPortResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state switchPortResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	sink := &diagSink{&resp.Diagnostics}
	siteID, siteName := r.resolveSite(ctx, state, sink)
	if resp.Diagnostics.HasError() {
		return
	}
	cur, err := r.data.client.GetSwitchPort(ctx, siteID, state.SwitchMAC.ValueString(), int(state.Port.ValueInt64()))
	if err != nil {
		// The switch is gone from the site, or the port number no longer
		// exists on it. Either way there is nothing left to manage.
		resp.State.RemoveResource(ctx)
		return
	}
	r.refresh(ctx, cur, &state)
	state.SiteID = types.StringValue(siteID)
	state.Site = types.StringValue(siteName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *switchPortResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state switchPortResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.SiteID = state.SiteID
	r.apply(ctx, &plan, &diagSink{&resp.Diagnostics})
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete drops the port from state without touching the switch.
//
// There is no "delete a physical port". Resetting it to some notional default
// on destroy would be worse than doing nothing: `terraform destroy` would
// silently reconfigure live switching, and the value to reset *to* would be
// this provider's guess rather than anything the practitioner chose.
func (r *switchPortResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning(
		"Switch port left as configured",
		"Ports are physical and cannot be deleted, so Terraform has only stopped managing this one. "+
			"Its settings on the switch are unchanged — reset them by hand, or re-import the port, if "+
			"that is not what you wanted.",
	)
}

// ImportState takes "<switch mac>/<port>", optionally prefixed with a site:
// "<site>/<switch mac>/<port>".
func (r *switchPortResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.Split(req.ID, "/")
	if len(parts) == 3 {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("site"), parts[0])...)
		parts = parts[1:]
	}
	if len(parts) != 2 {
		resp.Diagnostics.AddError(
			"Unexpected import id",
			fmt.Sprintf("expected \"<switch mac>/<port>\" or \"<site>/<switch mac>/<port>\", got %q", req.ID),
		)
		return
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Unexpected import id", fmt.Sprintf("port %q is not a number", parts[1]))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("switch_mac"), omada.NormalizeMAC(parts[0]))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("port"), int64(port))...)
}
