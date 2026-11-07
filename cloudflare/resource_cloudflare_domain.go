package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/hashicorp/terraform-plugin-framework-validators/float64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64default"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	cloudflareapi "github.com/myklst/terraform-provider-st-cloudflare/cloudflareAPI"
)

// Registration and workflow timeouts.
const (
	domainRegistrationTimeout = 5 * time.Minute
	domainUpdateTimeout       = 2 * time.Minute
)

type cloudflareDomainResource struct {
	client *cloudflare.Client
}

type cloudflareDomainModel struct {
	AccountID        types.String  `tfsdk:"account_id"`
	UseSandbox       types.Bool    `tfsdk:"use_sandbox"`
	Domain           types.String  `tfsdk:"domain"`
	PurchaseYears    types.Int64   `tfsdk:"purchase_years"`
	MaxPrice         types.Float64 `tfsdk:"max_price"`
	AutoRenew        types.Bool    `tfsdk:"auto_renew"`
	Privacy          types.Bool    `tfsdk:"privacy"`
	DomainExpiryDate types.String  `tfsdk:"domain_expiry_date"`
}

func NewCloudflareDomainResource() resource.Resource {
	return &cloudflareDomainResource{}
}

// Metadata
func (r *cloudflareDomainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain"
}

// Schema
func (r *cloudflareDomainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Register a domain through the Cloudflare Registrar. Requires the provider `api_token`. " +
			"When `use_sandbox` is set on the resource, it targets the Registrar Sandbox endpoints " +
			"(free test registrations, `.com`/`.net` only).",
		Attributes: map[string]schema.Attribute{
			"account_id": &schema.StringAttribute{
				MarkdownDescription: "The Cloudflare account ID that owns the registration. Changing this registers " +
					"the domain under a different account (forces replacement).",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"use_sandbox": &schema.BoolAttribute{
				MarkdownDescription: "Register through the Registrar Sandbox endpoints instead of the production " +
					"Registrar: free test registrations (`.com`/`.net` only), no billing, no real domains. " +
					"Defaults to `false`. Changing it forces replacement.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
				},
			},
			"domain": &schema.StringAttribute{
				MarkdownDescription: "Domain name to register (FQDN, e.g. `example.com`).",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"purchase_years": &schema.Int64Attribute{
				MarkdownDescription: "Registration term in years (1-10). Defaults to `1`. Cannot be changed after registration.",
				Optional:            true,
				Computed:            true,
				Default:             int64default.StaticInt64(1),
				Validators: []validator.Int64{
					int64validator.Between(1, 10),
				},
			},
			"max_price": &schema.Float64Attribute{
				MarkdownDescription: "Maximum price for the purchase. The provider checks the current first-year registrar " +
					"price and refuses to buy when it exceeds this value.",
				Required: true,
				Validators: []validator.Float64{
					float64validator.AtLeast(0),
				},
			},
			"auto_renew": &schema.BoolAttribute{
				MarkdownDescription: "Whether the domain is automatically renewed before expiration. " +
					"Setting `true` authorizes Cloudflare to charge the account's default payment method up to 30 days " +
					"before expiry.",
				Required: true,
			},
			"privacy": &schema.BoolAttribute{
				MarkdownDescription: "WHOIS privacy: `true` requests redaction, `false` disables it. " +
					"Cannot be changed after registration.",
				Required: true,
			},
			"domain_expiry_date": &schema.StringAttribute{
				MarkdownDescription: "The expiry date of the domain in ISO 8601 format (e.g. `2027-01-15T10:00:00Z`). " +
					"Computed automatically from the registration.",
				Computed: true,
			},
		},
	}
}

// Configure stores the shared cloudflare-go client; account_id and use_sandbox
// come from each resource instance.
func (r *cloudflareDomainResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		// this data available on apply stage
		return
	}
	clients, ok := req.ProviderData.(*providerClients)
	if !ok {
		resp.Diagnostics.AddError("req.ProviderData isn't a *providerClients", "")
		return
	}
	r.client = clients.cloudflare
}

// Create
func (r *cloudflareDomainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan *cloudflareDomainModel
	d := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	accountID := plan.AccountID.ValueString()
	useSandbox := plan.UseSandbox.ValueBool()
	domain := plan.Domain.ValueString()
	years := plan.PurchaseYears.ValueInt64()
	maxPrice := plan.MaxPrice.ValueFloat64()
	autoRenew := plan.AutoRenew.ValueBool()
	privacy := plan.Privacy.ValueBool()

	svc := cloudflareapi.NewRegistrar(r.client, accountID, useSandbox)

	// Pre-flight the price guard once: its refusals are deterministic, so it does
	// not belong inside the retry loop.
	if err := svc.CheckDomainPrice(ctx, domain, years, maxPrice); err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "domain [%s] creation failed", domain))
		return
	}

	tflog.Info(ctx, fmt.Sprintf("Domain [%s] is registering...", domain))
	createDomain := func() error {
		err := svc.CreateDomain(ctx, domain, autoRenew, privacy, years)
		if err == nil {
			return nil
		}
		// Deterministic failures never succeed on retry: 4xx API refusals (domain
		// taken, premium, unsupported TLD, auth) come back identical every time, and
		// a canceled context cannot recover. 429 and 5xx stay retryable.
		var cfErr *cloudflare.Error
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return backoff.Permanent(err)
		case errors.As(err, &cfErr) && cfErr.StatusCode >= 400 && cfErr.StatusCode < 500 && cfErr.StatusCode != http.StatusTooManyRequests:
			return backoff.Permanent(err)
		}
		return err
	}
	reconnectBackoff := backoff.NewExponentialBackOff()
	reconnectBackoff.MaxElapsedTime = 30 * time.Second
	err := backoff.Retry(createDomain, reconnectBackoff)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "domain [%s] creation failed after retries", domain))
		return
	}
	if err := svc.WaitWorkflow(ctx, domain, domainRegistrationTimeout); err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed", domain))
		return
	}

	state := &cloudflareDomainModel{
		AccountID:     plan.AccountID,
		UseSandbox:    plan.UseSandbox,
		Domain:        plan.Domain,
		PurchaseYears: plan.PurchaseYears,
		MaxPrice:      plan.MaxPrice,
		AutoRenew:     plan.AutoRenew,
		Privacy:       plan.Privacy,
	}
	_, _, expiry, err := svc.GetDomainInfo(ctx, domain)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		return
	}
	state.DomainExpiryDate = types.StringValue(expiry)
	tflog.Info(ctx, fmt.Sprintf("Domain [%s] registered, expires at [%s]", domain, state.DomainExpiryDate.ValueString()))

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// Read
func (r *cloudflareDomainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state *cloudflareDomainModel
	d := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	accountID := state.AccountID.ValueString()
	domain := state.Domain.ValueString()
	svc := cloudflareapi.NewRegistrar(r.client, accountID, state.UseSandbox.ValueBool())
	autoRenew, privacy, expiry, err := svc.GetDomainInfo(ctx, domain)
	if err != nil {
		var apiErr *cloudflare.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			resp.State.RemoveResource(ctx)
		} else {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		}
		return
	}
	state.AutoRenew = types.BoolValue(autoRenew)
	if state.Privacy.IsNull() {
		state.Privacy = types.BoolValue(privacy == "redaction")
	}
	state.DomainExpiryDate = types.StringValue(expiry)

	resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
}

// Update
func (r *cloudflareDomainResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan *cloudflareDomainModel
	d := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state *cloudflareDomainModel
	d = req.State.Get(ctx, &state)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	accountID := plan.AccountID.ValueString()
	domain := plan.Domain.ValueString()
	svc := cloudflareapi.NewRegistrar(r.client, accountID, plan.UseSandbox.ValueBool())

	// The Registrar API supports updating auto_renew only; everything else is set
	// at registration time and cannot be changed afterwards. purchase_years is
	// never returned by the API, so a null on either side means
	// "unknown/unmanaged" and the planned value is adopted rather than rejected as
	// an update.
	changed := []string{}
	if !plan.PurchaseYears.Equal(state.PurchaseYears) && !plan.PurchaseYears.IsNull() && !state.PurchaseYears.IsNull() {
		changed = append(changed, "purchase_years")
	}
	if !plan.Privacy.Equal(state.Privacy) {
		changed = append(changed, "privacy")
	}
	if len(changed) > 0 {
		resp.Diagnostics.AddError(
			"Unsupported domain update",
			fmt.Sprintf("The Cloudflare Registrar API supports updating `auto_renew` only, but the plan also changes: %s. "+
				"Revert these changes or recreate the resource.", strings.Join(changed, ", ")),
		)
		return
	}

	if !plan.AutoRenew.Equal(state.AutoRenew) {
		tflog.Info(ctx, fmt.Sprintf("Domain [%s] auto_renew update to [%t]...", domain, plan.AutoRenew.ValueBool()))
		if err := svc.SetAutoRenew(ctx, domain, plan.AutoRenew.ValueBool(), domainUpdateTimeout); err != nil {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "update domain [%s] failed", domain))
			return
		}
	}

	newState := &cloudflareDomainModel{
		AccountID:     plan.AccountID,
		UseSandbox:    plan.UseSandbox,
		Domain:        plan.Domain,
		PurchaseYears: plan.PurchaseYears,
		MaxPrice:      plan.MaxPrice,
		AutoRenew:     plan.AutoRenew,
		Privacy:       plan.Privacy,
	}
	autoRenew, _, expiry, err := svc.GetDomainInfo(ctx, domain)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		return
	}
	newState.AutoRenew = types.BoolValue(autoRenew)
	newState.DomainExpiryDate = types.StringValue(expiry)

	resp.Diagnostics.Append(resp.State.Set(ctx, newState)...)
}

// Delete
func (r *cloudflareDomainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state *cloudflareDomainModel
	getStateDiags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(getStateDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Since a domain registration can not be deleted in Cloudflare, the best we
	// can do is stop it from renewing: when auto_renew is enabled, turn it off so
	// the registration simply lapses at the end of its term.
	domain := state.Domain.ValueString()
	if state.AutoRenew.ValueBool() {
		svc := cloudflareapi.NewRegistrar(r.client, state.AccountID.ValueString(), state.UseSandbox.ValueBool())
		if err := svc.SetAutoRenew(ctx, domain, false, domainUpdateTimeout); err != nil {
			resp.Diagnostics.AddWarning(
				"Failed to disable auto_renew",
				fmt.Sprintf("Destroying [%s] but disabling auto_renew failed: %s. The domain stays registered and may still renew — disable auto_renew manually to stop renewal charges.", domain, err),
			)
		}
	}
	msg := fmt.Sprintf("Since a domain registration can not be deleted in Cloudflare, [%s] remains registered until its term ends.", domain)
	tflog.Warn(ctx, msg)
}

// ImportState expects the ID in the format <account_id>/<domain>. Sandbox
// registrations use the format sandbox/<account_id>/<domain>.
func (r *cloudflareDomainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id := req.ID
	useSandbox := false
	if strings.HasPrefix(id, "sandbox/") {
		useSandbox = true
		id = strings.TrimPrefix(id, "sandbox/")
	}
	parts := strings.SplitN(id, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected import ID in the format <account_id>/<domain> (or sandbox/<account_id>/<domain> for Registrar Sandbox registrations), got: %s", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("account_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("domain"), strings.ToLower(parts[1]))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("use_sandbox"), useSandbox)...)
}
