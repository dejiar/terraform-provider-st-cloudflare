package cloudflare

import (
	"context"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	"github.com/cloudflare/cloudflare-go/v7/zones"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource              = &zoneTypeResource{}
	_ resource.ResourceWithConfigure = &zoneTypeResource{}
)

func NewZoneTypeResource() resource.Resource {
	return &zoneTypeResource{}
}

type zoneTypeResource struct {
	client *cloudflare.Client
}

type zoneTypeResourceModel struct {
	ZoneId          types.String `tfsdk:"zone_id"`
	ZoneType        types.String `tfsdk:"zone_type"`
	ZonePlan        types.String `tfsdk:"zone_plan"`
	VerificationKey types.String `tfsdk:"verification_key"`
}

func (r *zoneTypeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_zone_type"
}

func (r *zoneTypeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Provide a Cloudflare zone plan resource.",
		Attributes: map[string]schema.Attribute{
			"zone_id": schema.StringAttribute{
				Description: "Cloudflare zone ID.",
				Required:    true,
			},
			"zone_type": schema.StringAttribute{
				Description: "Zone type." +
					"Valid value: partial, secondary, internal.",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf("partial", "secondary", "internal"),
				},
			},
			"zone_plan": schema.StringAttribute{
				Description: "Zone rate plan." +
					"Valid value: business, enterprise.",
				Required: true,
				Validators: []validator.String{
					stringvalidator.OneOf("business", "enterprise"),
				},
			},
			"verification_key": schema.StringAttribute{
				Description: "Verification key for partial zone setup.",
				Computed:    true,
			},
		},
	}
}

func (r *zoneTypeResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	clients, ok := req.ProviderData.(*providerClients)
	if !ok {
		resp.Diagnostics.AddError("req.ProviderData isn't a *providerClients", "")
		return
	}
	r.client = clients.cloudflare
}

func (r *zoneTypeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Retrieve values from plan
	var plan *zoneTypeResourceModel
	planDiags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(planDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	validation_key, err := r.updateZoneType(plan.ZoneId.ValueString(), plan.ZonePlan.ValueString(), plan.ZoneType.ValueString())
	if err != nil {
		resp.Diagnostics.Append(err)
		return
	}

	state := zoneTypeResourceModel{
		ZoneId:   plan.ZoneId,
		ZoneType: plan.ZoneType,
		ZonePlan: plan.ZonePlan,
	}

	// Get cloudflare validation key after convert to partial zone
	state.VerificationKey = types.StringValue(validation_key)

	setStateDiags := resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(setStateDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

func (r *zoneTypeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Get current state
	var state *zoneTypeResourceModel
	getStateDiags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(getStateDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	zone_id := state.ZoneId.ValueString()
	getResp, err := r.client.Zones.Get(context.TODO(), zones.ZoneGetParams{
		ZoneID: cloudflare.F(zone_id),
	})
	if err != nil {
		resp.Diagnostics.AddError("Get zone error ", err.Error())
		return
	}

	state.ZoneType = types.StringValue(string(getResp.Type))
	setStateDiags := resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(setStateDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

func (r *zoneTypeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Retrieve values from plan
	var plan *zoneTypeResourceModel
	planDiags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(planDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state := zoneTypeResourceModel{
		ZoneId:   plan.ZoneId,
		ZoneType: plan.ZoneType,
		ZonePlan: plan.ZonePlan,
	}

	validation_key, err := r.updateZoneType(plan.ZoneId.ValueString(), plan.ZonePlan.ValueString(), plan.ZoneType.ValueString())
	if err != nil {
		resp.Diagnostics.Append(err)
		return
	}

	// Get cloudflare validation key after convert to partial zone
	state.VerificationKey = types.StringValue(validation_key)

	setStateDiags := resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(setStateDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

func (r *zoneTypeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state *zoneTypeResourceModel
	planDiags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(planDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	zoneId := state.ZoneId.ValueString()

	_, err := r.client.Zones.Edit(context.TODO(), zones.ZoneEditParams{
		ZoneID: cloudflare.F(zoneId),
		Type:   cloudflare.F(zones.ZoneEditParamsType("full")),
	})
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "failed to set zone id [%s] type to full ", zoneId))
	}

	_, err = r.client.Zones.Subscriptions.New(
		context.TODO(),
		zones.SubscriptionNewParams{
			ZoneID: cloudflare.F(zoneId),
			Subscription: shared.SubscriptionParam{
				Frequency: cloudflare.F(shared.SubscriptionFrequencyMonthly),
				RatePlan: cloudflare.F(shared.RatePlanParam{
					ID: cloudflare.F(shared.RatePlanIDFree),
				}),
			},
		},
	)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "failed to set zone id [%s] to [%s] subscriptions", zoneId, "free"))
	}
}

func (r *zoneTypeResource) updateZoneType(zoneId string, zonePlan string, zoneType string) (string, diag.Diagnostic) {
	var zone *zones.Zone
	var err error

	// In order to change zone type to partial, zone rate plan has to change to
	// `business` or `enterprise` plan.
	getDomainExpiryInfo := func() error {
		_, err = r.client.Zones.Subscriptions.New(
			context.TODO(),
			zones.SubscriptionNewParams{
				ZoneID: cloudflare.F(zoneId),
				Subscription: shared.SubscriptionParam{
					Frequency: cloudflare.F(shared.SubscriptionFrequencyMonthly),
					RatePlan: cloudflare.F(shared.RatePlanParam{
						ID: cloudflare.F(shared.RatePlanID(zonePlan)),
					}),
				},
			},
		)
		if err != nil {
			return fmt.Errorf("failed to set zone id [%s] to [%s] subscriptions", zoneId, zonePlan)
		}

		zone, err = r.client.Zones.Edit(context.TODO(), zones.ZoneEditParams{
			ZoneID: cloudflare.F(zoneId),
			Type:   cloudflare.F(zones.ZoneEditParamsType(zoneType)),
		})
		if err != nil {
			return fmt.Errorf("failed to set zone id [%s] to [%s]", zoneId, zoneType)
		}

		return nil
	}

	reconnectBackoff := backoff.NewExponentialBackOff()
	reconnectBackoff.MaxElapsedTime = 30 * time.Second
	_err := backoff.Retry(getDomainExpiryInfo, reconnectBackoff)
	if _err != nil {
		return "", diagnosticErrorOf(err, "failed to update domain type for [%s] after retries", zoneId)
	}

	return zone.VerificationKey, nil
}

func diagnosticErrorOf(err error, format string, a ...any) diag.Diagnostic {
	msg := fmt.Sprintf(format, a...)
	if err != nil {
		return diag.NewErrorDiagnostic(msg, err.Error())
	} else {
		return diag.NewErrorDiagnostic(msg, "")
	}
}
