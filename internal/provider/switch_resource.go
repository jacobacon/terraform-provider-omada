// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/wncservices/terraform-provider-omada/internal/omada"
)

var (
	_ resource.Resource                = &switchResource{}
	_ resource.ResourceWithConfigure   = &switchResource{}
	_ resource.ResourceWithImportState = &switchResource{}
)

func NewSwitchResource() resource.Resource { return &switchResource{} }

// switchResource manages device-level settings on an adopted switch, as
// omada_gateway and omada_access_point do for theirs. Today that is only the
// name; its ports are omada_switch_port.
type switchResource struct{ data *providerData }

type switchResourceModel struct {
	ID     types.String `tfsdk:"id"`
	Site   types.String `tfsdk:"site"`
	SiteID types.String `tfsdk:"site_id"`
	MAC    macValue     `tfsdk:"mac"`
	Name   types.String `tfsdk:"name"`
	Model  types.String `tfsdk:"model"`
}

func (r *switchResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_switch"
}

func (r *switchResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages device-level settings on an adopted switch. Today that is its name; " +
			"ports are managed with `omada_switch_port`.\n\n" +
			"~> **A switch cannot be created or destroyed.** Terraform *adopts* the existing " +
			"device. Removing this resource from your configuration leaves it exactly as last " +
			"applied: Terraform forgets it, the controller does not.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The switch MAC, normalised.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"site": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Site name. Defaults to the primary site. Changing forces replacement.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace(),
				},
			},
			"site_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"mac": schema.StringAttribute{
				CustomType: macType{},
				Required:   true,
				MarkdownDescription: "MAC address of the switch, in any common spelling " +
					"(`8C-86-DD-10-50-CA`, `8c:86:dd:10:50:ca`). See the `omada_devices` data source. " +
					"Changing forces replacement.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"name": schema.StringAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Device name shown in the controller. Leave it out to keep the current one.",
			},
			"model": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Hardware model, e.g. `ES206X-M2`.",
			},
		},
	}
}

func (r *switchResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *switchResource) resolveSite(ctx context.Context, m switchResourceModel, diags *diagSink) (string, string) {
	name := r.data.defaultSite
	if !m.Site.IsNull() && m.Site.ValueString() != "" {
		name = m.Site.ValueString()
	}
	site, err := r.data.client.ResolveSite(ctx, name)
	if err != nil {
		diags.AddError("Unable to resolve site", err.Error())
		return "", ""
	}
	return site.ID, site.Name
}

// getSwitch reads the device and refuses one that is not a switch, since the
// other device types have their own resources and document paths.
func (r *switchResource) getSwitch(ctx context.Context, siteID, mac string) (*omada.Device, error) {
	d, err := r.data.client.GetDevice(ctx, siteID, mac)
	if err != nil {
		return nil, err
	}
	if d.Type != "switch" {
		return nil, fmt.Errorf("device %s is a %q, not a switch; use omada_access_point or omada_gateway for it",
			omada.NormalizeMAC(mac), d.Type)
	}
	return d, nil
}

func (m *switchResourceModel) refresh(d *omada.Device, siteID, siteName string) {
	m.ID = types.StringValue(omada.NormalizeMAC(d.MAC))
	m.Name = types.StringValue(d.Name)
	m.Model = types.StringValue(d.Model)
	m.SiteID = types.StringValue(siteID)
	m.Site = types.StringValue(siteName)
}

// apply is shared by Create and Update. The name is only written when it
// differs from the controller's, so adopting a switch under its current name
// sends nothing. After a rename the switch is read again, so state holds what
// the controller stored.
func (r *switchResource) apply(ctx context.Context, plan *switchResourceModel, diags *diagSink) {
	siteID, siteName := r.resolveSite(ctx, *plan, diags)
	if siteID == "" {
		return
	}
	mac := plan.MAC.ValueString()

	cur, err := r.getSwitch(ctx, siteID, mac)
	if err != nil {
		diags.AddError("Unable to read switch", err.Error())
		return
	}

	if known(plan.Name) && plan.Name.ValueString() != cur.Name {
		if err := r.data.client.RenameSwitch(ctx, siteID, mac, plan.Name.ValueString()); err != nil {
			diags.AddError("Unable to rename switch", err.Error())
			return
		}
		if cur, err = r.getSwitch(ctx, siteID, mac); err != nil {
			diags.AddError("Unable to read switch after rename", err.Error())
			return
		}
	}

	plan.refresh(cur, siteID, siteName)
}

// Create adopts an existing switch rather than making one.
func (r *switchResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan switchResourceModel
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

func (r *switchResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state switchResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	siteID, siteName := r.resolveSite(ctx, state, &diagSink{&resp.Diagnostics})
	if resp.Diagnostics.HasError() {
		return
	}
	cur, err := r.getSwitch(ctx, siteID, state.MAC.ValueString())
	if errors.Is(err, omada.ErrDeviceNotFound) {
		// Removed from the site: nothing left to manage.
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read switch", err.Error())
		return
	}
	state.refresh(cur, siteID, siteName)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *switchResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan switchResourceModel
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

// Delete drops the switch from state without touching the device.
func (r *switchResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning(
		"Switch left as configured",
		"A switch cannot be deleted, so Terraform has only stopped managing this one. Its name "+
			"on the controller is unchanged and it is still adopted.",
	)
}

// ImportState takes the switch MAC, or "<site>/<mac>".
func (r *switchResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := req.ID
	if site, rest, found := strings.Cut(id, "/"); found {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("site"), site)...)
		id = rest
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("mac"), omada.NormalizeMAC(id))...)
}
