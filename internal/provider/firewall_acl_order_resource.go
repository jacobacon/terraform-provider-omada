// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/wncservices/terraform-provider-omada/internal/omada"
)

var (
	_ resource.Resource                = &firewallACLOrderResource{}
	_ resource.ResourceWithConfigure   = &firewallACLOrderResource{}
	_ resource.ResourceWithImportState = &firewallACLOrderResource{}
)

func NewFirewallACLOrderResource() resource.Resource {
	return &firewallACLOrderResource{}
}

// orderableACLTypes are the ACL types whose ordering has been verified on a live
// controller. modifyIndex takes a numeric type and switch (1) and EAP (2) are
// presumably accepted, but only gateway (0) has been exercised; widen this once
// the others have been, rather than shipping an ordering nobody has watched move.
var orderableACLTypes = []string{"gateway"}

type firewallACLOrderResource struct {
	data *providerData
}

type firewallACLOrderModel struct {
	ID      types.String `tfsdk:"id"`
	Site    types.String `tfsdk:"site"`
	SiteID  types.String `tfsdk:"site_id"`
	Type    types.String `tfsdk:"type"`
	RuleIDs types.List   `tfsdk:"rule_ids"`
}

func (r *firewallACLOrderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_firewall_acl_order"
}

func (r *firewallACLOrderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Controls the **evaluation order** of firewall ACL rules. " +
			"Omada evaluates gateway ACLs top to bottom and the first match wins, so a permit " +
			"below a deny that also matches is dead. A created rule always appends to the end of " +
			"the list, which makes that the default outcome.\n\n" +
			"The listed rules are arranged into this relative order **among the positions they " +
			"already occupy**. Rules at positions 2, 5 and 7 can be rearranged among 2, 5 and 7, " +
			"and nothing else on the controller moves — so a config need not account for rules it " +
			"does not manage. The flip side is that a rule cannot be moved past a rule that is not " +
			"listed; to do that, list that rule too. Listing every rule of the type gives complete " +
			"control of the order.\n\n" +
			"One instance per site and ACL type. `rule_ids` is an order, not a set: changing it " +
			"updates in place, and destroying the resource stops managing the order without " +
			"changing it on the controller.\n\n" +
			"An ACL of the same type that is created or deleted in the same apply can shift the " +
			"positions between reading and writing them; the controller then refuses the reorder " +
			"without moving anything, and running apply again completes it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "`<site_id>/<type>` — the order is a per-site, per-type singleton.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"site": schema.StringAttribute{
				MarkdownDescription: "Site name. Defaults to the primary site. Changing this forces replacement.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(), stringplanmodifier.RequiresReplace(),
				},
			},
			"site_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"type": schema.StringAttribute{
				MarkdownDescription: "ACL type. Only `gateway` is accepted: `modifyIndex` has been verified " +
					"live for gateway ACLs only, and switch and EAP ACLs are not yet supported. " +
					"Changing forces replacement.",
				Optional:      true,
				Computed:      true,
				Default:       stringdefault.StaticString("gateway"),
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{stringvalidator.OneOf(orderableACLTypes...)},
			},
			"rule_ids": schema.ListAttribute{
				MarkdownDescription: "ACL rule IDs in the order they should be evaluated, first match first. " +
					"Reference the rules rather than hardcoding IDs (`omada_firewall_acl.x.id`) so Terraform " +
					"creates them before ordering them.",
				ElementType: types.StringType,
				Required:    true,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.UniqueValues(),
				},
			},
		},
	}
}

func (r *firewallACLOrderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *providerData, got %T", req.ProviderData))
		return
	}
	r.data = data
}

func (r *firewallACLOrderResource) siteName(m firewallACLOrderModel) string {
	if !m.Site.IsNull() && m.Site.ValueString() != "" {
		return m.Site.ValueString()
	}
	return r.data.defaultSite
}

func (r *firewallACLOrderResource) aclType(m firewallACLOrderModel) (int, bool) {
	t, ok := aclTypeToInt[strings.ToLower(m.Type.ValueString())]
	return t, ok
}

// apply writes the order the controller actually holds for the managed rules.
//
// Only the listed rules are refreshed, and only their RELATIVE order, because
// that is all this resource controls. Any id that is no longer on the controller
// is dropped, which surfaces as a diff against the configuration rather than a
// silent success.
func (r *firewallACLOrderResource) apply(ctx context.Context, site omada.Site, aclType int, m *firewallACLOrderModel) error {
	siteID := site.ID
	typeName, ok := aclTypeToStr[aclType]
	if !ok {
		return fmt.Errorf("unknown acl type %d", aclType)
	}
	want, diags := stringSlice(ctx, m.RuleIDs)
	if diags.HasError() {
		return diagsError("reading rule_ids", diags)
	}
	acls, err := r.data.client.ListACLs(ctx, siteID, aclType)
	if err != nil {
		return err
	}
	byID := make(map[string]omada.ACL, len(acls))
	for _, a := range acls {
		byID[a.ID] = a
	}
	live := make([]omada.ACL, 0, len(want))
	for _, id := range want {
		if a, ok := byID[id]; ok {
			live = append(live, a)
		}
	}
	// Sorting equal or missing indexes would just echo the configured order
	// back, hiding both a failed reorder and out-of-band drift.
	if err := omada.CheckACLOrder(live); err != nil {
		return err
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Index < live[j].Index })

	ids := make([]string, 0, len(live))
	for _, a := range live {
		ids = append(ids, a.ID)
	}
	lv, d := stringListValue(ctx, ids)
	if d.HasError() {
		return diagsError("building rule_ids", d)
	}
	m.RuleIDs = lv
	m.Site = types.StringValue(site.Name)
	m.SiteID = types.StringValue(siteID)
	m.Type = types.StringValue(typeName)
	m.ID = types.StringValue(siteID + "/" + typeName)
	return nil
}

// diagsError folds framework diagnostics into an error so the underlying
// detail reaches the user instead of a bare "could not read" message.
func diagsError(what string, diags diag.Diagnostics) error {
	msgs := make([]string, 0, len(diags.Errors()))
	for _, e := range diags.Errors() {
		msgs = append(msgs, e.Summary()+": "+e.Detail())
	}
	return fmt.Errorf("%s: %s", what, strings.Join(msgs, "; "))
}

// reorder is Create and Update: both just assert the configured order.
func (r *firewallACLOrderResource) reorder(ctx context.Context, plan *firewallACLOrderModel) (omada.Site, int, error) {
	site, err := r.data.client.ResolveSite(ctx, r.siteName(*plan))
	if err != nil {
		return omada.Site{}, 0, fmt.Errorf("unable to resolve site: %w", err)
	}
	aclType, ok := r.aclType(*plan)
	if !ok {
		return omada.Site{}, 0, fmt.Errorf("type must be one of %v", orderableACLTypes)
	}
	ids, diags := stringSlice(ctx, plan.RuleIDs)
	if diags.HasError() {
		return omada.Site{}, 0, diagsError("reading rule_ids", diags)
	}
	if err := r.data.client.ReorderACLs(ctx, site.ID, aclType, ids); err != nil {
		return omada.Site{}, 0, err
	}
	return site, aclType, nil
}

func (r *firewallACLOrderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan firewallACLOrderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	site, aclType, err := r.reorder(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to order ACL rules", err.Error())
		return
	}
	if err := r.apply(ctx, site, aclType, &plan); err != nil {
		resp.Diagnostics.AddError("Unable to read back ACL order", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *firewallACLOrderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state firewallACLOrderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	site, err := r.data.client.ResolveSite(ctx, r.siteName(state))
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve site", err.Error())
		return
	}
	aclType, ok := r.aclType(state)
	if !ok {
		aclType = omada.ACLTypeGateway
	}
	if err := r.apply(ctx, site, aclType, &state); err != nil {
		resp.Diagnostics.AddError("Unable to read ACL order", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *firewallACLOrderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan firewallACLOrderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	site, aclType, err := r.reorder(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Unable to order ACL rules", err.Error())
		return
	}
	if err := r.apply(ctx, site, aclType, &plan); err != nil {
		resp.Diagnostics.AddError("Unable to read back ACL order", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete is intentionally a no-op on the controller.
//
// An ordering cannot be removed — the rules keep whatever positions they hold.
// Destroying this resource means "stop managing the order", and reordering the
// rules back to some prior arrangement would be a guess at an order Terraform
// never recorded.
func (r *firewallACLOrderResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

// ImportState accepts "<type>" or "<site_name>/<type>", e.g. "gateway".
//
// There is nothing else to address: the order is a per-site, per-type singleton.
// The import brings in no rule_ids — which rules the configuration intends to
// order is not discoverable from the controller — so an import is followed by a
// one-time diff that adds them, as with the other sparse resources.
func (r *firewallACLOrderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	site, aclType := "", req.ID
	if parts := strings.SplitN(req.ID, "/", 2); len(parts) == 2 {
		site, aclType = parts[0], parts[1]
	}
	if !slices.Contains(orderableACLTypes, strings.ToLower(aclType)) {
		resp.Diagnostics.AddError("Invalid import ID",
			fmt.Sprintf("expected %q or %q, where type is one of %v; got %q",
				"<type>", "<site>/<type>", orderableACLTypes, req.ID))
		return
	}
	if site != "" {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("site"), site)...)
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("type"), strings.ToLower(aclType))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("rule_ids"), []string{})...)
}
