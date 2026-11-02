package cloudflare

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/myklst/terraform-provider-st-cloudflare/cloudflare/sdk"
)

// Registration and workflow timeouts.
const (
	domainRegistrationTimeout = 5 * time.Minute
	domainUpdateTimeout       = 2 * time.Minute
)

type cloudflareDomainResource struct {
	client *sdk.Client
}

func NewCloudflareDomainResource() resource.Resource {
	return &cloudflareDomainResource{}
}

type registrantContactModel struct {
	Email        types.String `tfsdk:"email"`
	Phone        types.String `tfsdk:"phone"`
	Fax          types.String `tfsdk:"fax"`
	Name         types.String `tfsdk:"name"`
	Organization types.String `tfsdk:"organization"`
	Street       types.String `tfsdk:"street"`
	City         types.String `tfsdk:"city"`
	State        types.String `tfsdk:"state"`
	PostalCode   types.String `tfsdk:"postal_code"`
	CountryCode  types.String `tfsdk:"country_code"`
}

type cloudflareDomainModel struct {
	Domain            types.String            `tfsdk:"domain"`
	PurchaseYears     types.Int64             `tfsdk:"purchase_years"`
	AutoRenew         types.Bool              `tfsdk:"auto_renew"`
	Privacy           types.Bool              `tfsdk:"privacy"`
	RegistrantContact *registrantContactModel `tfsdk:"registrant_contact"`
	DomainExpiryDate  types.String            `tfsdk:"domain_expiry_date"`
	Status            types.String            `tfsdk:"status"`
}

// Metadata
func (r *cloudflareDomainResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_domain"
}

// Schema
func (r *cloudflareDomainResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Register a domain through the Cloudflare Registrar. Requires the provider `api_token` and `account_id`. " +
			"When the provider `use_sandbox` flag is set, the resource targets the Registrar Sandbox endpoints " +
			"(free test registrations, `.com`/`.net` only).",
		Attributes: map[string]schema.Attribute{
			"domain": &schema.StringAttribute{
				MarkdownDescription: "Domain name to register (FQDN, e.g. `example.com`).",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"purchase_years": &schema.Int64Attribute{
				MarkdownDescription: "Registration term in years (1-10). Defaults to the registry's minimum period " +
					"(1 year for most extensions). Cannot be changed after registration.",
				Optional: true,
				Validators: []validator.Int64{
					int64validator.Between(1, 10),
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
			"registrant_contact": &schema.SingleNestedAttribute{
				MarkdownDescription: "Registrant contact for the registration. When omitted, the account's default " +
					"address book entry is used; without either, registration fails. Sandbox mode always requires " +
					"a contact.",
				Optional: true,
				Attributes: map[string]schema.Attribute{
					"email": &schema.StringAttribute{
						MarkdownDescription: "Email address of the registrant.",
						Required:            true,
					},
					"phone": &schema.StringAttribute{
						MarkdownDescription: "Phone number in E.164 format, e.g. `+1.5555555555`.",
						Required:            true,
					},
					"fax": &schema.StringAttribute{
						MarkdownDescription: "Fax number in E.164 format. Optional.",
						Optional:            true,
					},
					"name": &schema.StringAttribute{
						MarkdownDescription: "Full legal name of the contact in one string, e.g. `Ada Lovelace`.",
						Required:            true,
					},
					"organization": &schema.StringAttribute{
						MarkdownDescription: "Organization or company name. Optional for individual registrants.",
						Optional:            true,
					},
					"street": &schema.StringAttribute{
						MarkdownDescription: "Street address including building/suite number.",
						Required:            true,
					},
					"city": &schema.StringAttribute{
						MarkdownDescription: "City or locality name.",
						Required:            true,
					},
					"state": &schema.StringAttribute{
						MarkdownDescription: "State, province, or region (standard abbreviation, e.g. `TX`).",
						Optional:            true,
					},
					"postal_code": &schema.StringAttribute{
						MarkdownDescription: "Postal or ZIP code.",
						Required:            true,
					},
					"country_code": &schema.StringAttribute{
						MarkdownDescription: "Two-letter country code per ISO 3166-1 alpha-2 (e.g. `US`).",
						Required:            true,
					},
				},
			},
			"domain_expiry_date": &schema.StringAttribute{
				MarkdownDescription: "The expiry date of the domain in ISO 8601 format (e.g. `2027-01-15T10:00:00Z`). " +
					"Computed automatically from the registration.",
				Computed: true,
			},
			"status": &schema.StringAttribute{
				MarkdownDescription: "Registration status, e.g. `active`, `expired`, `suspended`.",
				Computed:            true,
			},
		},
	}
}

// Configure adds the provider configured client to the resource.
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
	r.client = clients.registrar
}

// Create
func (r *cloudflareDomainResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan *cloudflareDomainModel
	d := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.validateClient(); err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "provider is not ready to register domains"))
		return
	}

	domain := plan.Domain.ValueString()
	params := sdk.RegistrationCreateParams{
		DomainName:  domain,
		AutoRenew:   plan.AutoRenew.ValueBool(),
		PrivacyMode: privacyModeOf(plan.Privacy.ValueBool()),
	}
	if !plan.PurchaseYears.IsNull() {
		params.Years = plan.PurchaseYears.ValueInt64()
	}
	if plan.RegistrantContact != nil {
		params.Contacts = &sdk.RegistrationContacts{
			Registrant: registrantContactOf(plan.RegistrantContact),
		}
	}

	tflog.Info(ctx, fmt.Sprintf("Domain [%s] is registering...", domain))
	workflow, err := r.client.RegistrationsCreate(ctx, params)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed", domain))
		return
	}

	if _, err := r.client.WaitRegistrationWorkflow(ctx, workflow, domainRegistrationTimeout); err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed", domain))
		return
	}

	registration, err := r.client.RegistrationsGet(ctx, domain)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		return
	}
	tflog.Info(ctx, fmt.Sprintf("Domain [%s] registered, expires at [%s]", domain, registration.ExpiresAt))

	state := cloudflareDomainModel{
		Domain:            plan.Domain,
		PurchaseYears:     plan.PurchaseYears,
		AutoRenew:         plan.AutoRenew,
		Privacy:           plan.Privacy,
		RegistrantContact: plan.RegistrantContact,
		DomainExpiryDate:  types.StringValue(registration.ExpiresAt),
		Status:            types.StringValue(registration.Status),
	}

	d2 := resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(d2...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Read
func (r *cloudflareDomainResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state *cloudflareDomainModel
	d := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}

	domain := state.Domain.ValueString()
	registration, err := r.client.RegistrationsGet(ctx, domain)
	if err != nil {
		if strings.Contains(err.Error(), "Domain not found") {
			resp.State.RemoveResource(ctx)
		} else {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		}
		return
	}

	state.AutoRenew = types.BoolValue(registration.AutoRenew)
	state.DomainExpiryDate = types.StringValue(registration.ExpiresAt)
	state.Status = types.StringValue(registration.Status)

	d1 := resp.State.Set(ctx, state)
	resp.Diagnostics.Append(d1...)
	if resp.Diagnostics.HasError() {
		return
	}
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

	domain := plan.Domain.ValueString()

	// The Registrar API supports updating auto_renew only; everything else is
	// set at registration time and cannot be changed afterwards.
	changed := []string{}
	if !plan.PurchaseYears.Equal(state.PurchaseYears) {
		changed = append(changed, "purchase_years")
	}
	if !plan.Privacy.Equal(state.Privacy) {
		changed = append(changed, "privacy")
	}
	if !registrantContactEqual(plan.RegistrantContact, state.RegistrantContact) {
		changed = append(changed, "registrant_contact")
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
		workflow, err := r.client.RegistrationsEdit(ctx, domain, plan.AutoRenew.ValueBool())
		if err != nil {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "update domain [%s] failed", domain))
			return
		}
		if _, err := r.client.WaitRegistrationWorkflow(ctx, workflow, domainUpdateTimeout); err != nil {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "update domain [%s] failed", domain))
			return
		}
	}

	registration, err := r.client.RegistrationsGet(ctx, domain)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		return
	}

	newState := cloudflareDomainModel{
		Domain:            plan.Domain,
		PurchaseYears:     plan.PurchaseYears,
		AutoRenew:         types.BoolValue(registration.AutoRenew),
		Privacy:           plan.Privacy,
		RegistrantContact: plan.RegistrantContact,
		DomainExpiryDate:  types.StringValue(registration.ExpiresAt),
		Status:            types.StringValue(registration.Status),
	}

	setStateDiags := resp.State.Set(ctx, &newState)
	resp.Diagnostics.Append(setStateDiags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Delete
func (r *cloudflareDomainResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state *cloudflareDomainModel
	getStateDiags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(getStateDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Since a domain registration can not be deleted in Cloudflare, we do
	// nothing here but give a warning. The domain stays registered and
	// auto_renew keeps applying until it is turned off.
	domain := state.Domain.ValueString()
	msg := fmt.Sprintf("Since a domain registration can not be deleted in Cloudflare, [%s] still exists actually. Disable auto_renew to stop renewal charges.", domain)
	tflog.Warn(ctx, msg)
}

// ImportState
func (r *cloudflareDomainResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("domain"), req, resp)
}

func (r *cloudflareDomainResource) validateClient() error {
	if r.client == nil {
		return fmt.Errorf("registrar client is not configured")
	}
	if r.client.APIToken == "" {
		return fmt.Errorf("st-cloudflare_domain requires the provider `api_token` (API tokens only; email + api_key is not supported)")
	}
	if r.client.AccountID == "" {
		return fmt.Errorf("st-cloudflare_domain requires the provider `account_id`")
	}
	return nil
}

func privacyModeOf(privacy bool) string {
	if privacy {
		return sdk.PrivacyModeRedaction
	}
	return sdk.PrivacyModeOff
}

func registrantContactOf(m *registrantContactModel) *sdk.RegistrationContact {
	return &sdk.RegistrationContact{
		Email: m.Email.ValueString(),
		Phone: m.Phone.ValueString(),
		Fax:   m.Fax.ValueString(),
		PostalInfo: sdk.RegistrationContactPostalInfo{
			Address: sdk.RegistrationContactAddress{
				Street:      m.Street.ValueString(),
				City:        m.City.ValueString(),
				State:       m.State.ValueString(),
				PostalCode:  m.PostalCode.ValueString(),
				CountryCode: m.CountryCode.ValueString(),
			},
			Name:         m.Name.ValueString(),
			Organization: m.Organization.ValueString(),
		},
	}
}

func registrantContactEqual(a *registrantContactModel, b *registrantContactModel) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Email.Equal(b.Email) &&
		a.Phone.Equal(b.Phone) &&
		a.Fax.Equal(b.Fax) &&
		a.Name.Equal(b.Name) &&
		a.Organization.Equal(b.Organization) &&
		a.Street.Equal(b.Street) &&
		a.City.Equal(b.City) &&
		a.State.Equal(b.State) &&
		a.PostalCode.Equal(b.PostalCode) &&
		a.CountryCode.Equal(b.CountryCode)
}
