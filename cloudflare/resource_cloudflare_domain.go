package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/registrar"
	"github.com/cloudflare/cloudflare-go/v7/registrar_sandbox"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Registration and workflow timeouts.
const (
	domainRegistrationTimeout = 5 * time.Minute
	domainUpdateTimeout       = 2 * time.Minute
)

type cloudflareDomainResource struct {
	client *cloudflare.Client
}

// registrarFor builds the registrar service for one account: production
// Registrar or Registrar Sandbox, per the resource use_sandbox attribute.
func (r *cloudflareDomainResource) registrarFor(accountID string, useSandbox bool) registrarService {
	return &registrarAdapter{client: r.client, accountID: accountID, sandbox: useSandbox}
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
	AccountID         types.String            `tfsdk:"account_id"`
	UseSandbox        types.Bool              `tfsdk:"use_sandbox"`
	Domain            types.String            `tfsdk:"domain"`
	PurchaseYears     types.Int64             `tfsdk:"purchase_years"`
	AutoRenew         types.Bool              `tfsdk:"auto_renew"`
	Privacy           types.Bool              `tfsdk:"privacy"`
	RegistrantContact *registrantContactModel `tfsdk:"registrant_contact"`
	DomainExpiryDate  types.String            `tfsdk:"domain_expiry_date"`
	Status            types.String            `tfsdk:"status"`
}

// registrationView is the provider-agnostic view of a domain registration,
// shared by the production and sandbox registrar services.
type registrationView struct {
	autoRenew bool
	privacy   bool
	expiresAt string
	status    string
}

// workflowView is the provider-agnostic view of an async registrar workflow.
type workflowView struct {
	completed bool
	state     string
	errMsg    string
}

// createRegistrationOptions carries the resource plan into the registrar
// adapters.
type createRegistrationOptions struct {
	domainName string
	autoRenew  bool
	privacy    bool
	years      int64
	contact    *registrantContactModel
}

// registrarService abstracts the production Registrar service and the
// Registrar Sandbox service behind one shape so the resource code is
// identical for both.
type registrarService interface {
	CreateRegistration(ctx context.Context, opts createRegistrationOptions) (workflowView, error)
	GetRegistration(ctx context.Context, domainName string) (registrationView, error)
	EditRegistration(ctx context.Context, domainName string, autoRenew bool) (workflowView, error)
	GetRegistrationStatus(ctx context.Context, domainName string) (workflowView, error)
}

// registrarAdapter talks to the Registrar API: the production endpoints
// (/accounts/{account_id}/registrar/*) or, when sandbox is set, the Registrar
// Sandbox (/accounts/{account_id}/registrar-sandbox/*).
type registrarAdapter struct {
	client    *cloudflare.Client
	accountID string
	sandbox   bool
}

func (a *registrarAdapter) CreateRegistration(ctx context.Context, opts createRegistrationOptions) (workflowView, error) {
	if a.sandbox {
		params := registrar_sandbox.RegistrationNewParams{
			AccountID:   cloudflare.F(a.accountID),
			DomainName:  cloudflare.F(opts.domainName),
			AutoRenew:   cloudflare.F(opts.autoRenew),
			PrivacyMode: cloudflare.F(registrar_sandbox.RegistrationNewParamsPrivacyMode(privacyModeOf(opts.privacy))),
		}
		if opts.years > 0 {
			params.Years = cloudflare.F(opts.years)
		}
		if opts.contact != nil {
			c := contactOf(opts.contact)
			params.Contacts = cloudflare.F(registrar_sandbox.RegistrationNewParamsContacts{
				Registrant: cloudflare.F(registrar_sandbox.RegistrationNewParamsContactsRegistrant{
					Email: cloudflare.F(c.email),
					Phone: cloudflare.F(c.phone),
					Fax:   cloudflare.F(c.fax),
					PostalInfo: cloudflare.F(registrar_sandbox.RegistrationNewParamsContactsRegistrantPostalInfo{
						Address: cloudflare.F(registrar_sandbox.RegistrationNewParamsContactsRegistrantPostalInfoAddress{
							Street:      cloudflare.F(c.street),
							City:        cloudflare.F(c.city),
							State:       cloudflare.F(c.state),
							PostalCode:  cloudflare.F(c.postalCode),
							CountryCode: cloudflare.F(c.countryCode),
						}),
						Name:         cloudflare.F(c.name),
						Organization: cloudflare.F(c.organization),
					}),
				}),
			})
		}
		ws, err := a.client.RegistrarSandbox.Registrations.New(ctx, params)
		if err != nil {
			return workflowView{}, err
		}
		return workflowView{completed: ws.Completed, state: string(ws.State), errMsg: ws.Error.Message}, nil
	}
	params := registrar.RegistrationNewParams{
		AccountID:   cloudflare.F(a.accountID),
		DomainName:  cloudflare.F(opts.domainName),
		AutoRenew:   cloudflare.F(opts.autoRenew),
		PrivacyMode: cloudflare.F(registrar.RegistrationNewParamsPrivacyMode(privacyModeOf(opts.privacy))),
	}
	if opts.years > 0 {
		params.Years = cloudflare.F(opts.years)
	}
	if opts.contact != nil {
		c := contactOf(opts.contact)
		params.Contacts = cloudflare.F(registrar.RegistrationNewParamsContacts{
			Registrant: cloudflare.F(registrar.RegistrationNewParamsContactsRegistrant{
				Email: cloudflare.F(c.email),
				Phone: cloudflare.F(c.phone),
				Fax:   cloudflare.F(c.fax),
				PostalInfo: cloudflare.F(registrar.RegistrationNewParamsContactsRegistrantPostalInfo{
					Address: cloudflare.F(registrar.RegistrationNewParamsContactsRegistrantPostalInfoAddress{
						Street:      cloudflare.F(c.street),
						City:        cloudflare.F(c.city),
						State:       cloudflare.F(c.state),
						PostalCode:  cloudflare.F(c.postalCode),
						CountryCode: cloudflare.F(c.countryCode),
					}),
					Name:         cloudflare.F(c.name),
					Organization: cloudflare.F(c.organization),
				}),
			}),
		})
	}
	ws, err := a.client.Registrar.Registrations.New(ctx, params)
	if err != nil {
		return workflowView{}, err
	}
	return workflowView{completed: ws.Completed, state: string(ws.State), errMsg: ws.Error.Message}, nil
}

func (a *registrarAdapter) GetRegistration(ctx context.Context, domainName string) (registrationView, error) {
	if a.sandbox {
		reg, err := a.client.RegistrarSandbox.Registrations.Get(ctx, domainName, registrar_sandbox.RegistrationGetParams{
			AccountID: cloudflare.F(a.accountID),
		})
		if err != nil {
			return registrationView{}, err
		}
		return registrationOf(reg.AutoRenew, string(reg.PrivacyMode), reg.ExpiresAt, string(reg.Status)), nil
	}
	reg, err := a.client.Registrar.Registrations.Get(ctx, domainName, registrar.RegistrationGetParams{
		AccountID: cloudflare.F(a.accountID),
	})
	if err != nil {
		return registrationView{}, err
	}
	return registrationOf(reg.AutoRenew, string(reg.PrivacyMode), reg.ExpiresAt, string(reg.Status)), nil
}

func (a *registrarAdapter) EditRegistration(ctx context.Context, domainName string, autoRenew bool) (workflowView, error) {
	if a.sandbox {
		ws, err := a.client.RegistrarSandbox.Registrations.Edit(ctx, domainName, registrar_sandbox.RegistrationEditParams{
			AccountID: cloudflare.F(a.accountID),
			AutoRenew: cloudflare.F(autoRenew),
		})
		if err != nil {
			return workflowView{}, err
		}
		return workflowView{completed: ws.Completed, state: string(ws.State), errMsg: ws.Error.Message}, nil
	}
	ws, err := a.client.Registrar.Registrations.Edit(ctx, domainName, registrar.RegistrationEditParams{
		AccountID: cloudflare.F(a.accountID),
		AutoRenew: cloudflare.F(autoRenew),
	})
	if err != nil {
		return workflowView{}, err
	}
	return workflowView{completed: ws.Completed, state: string(ws.State), errMsg: ws.Error.Message}, nil
}

func (a *registrarAdapter) GetRegistrationStatus(ctx context.Context, domainName string) (workflowView, error) {
	if a.sandbox {
		ws, err := a.client.RegistrarSandbox.RegistrationStatus.Get(ctx, domainName, registrar_sandbox.RegistrationStatusGetParams{
			AccountID: cloudflare.F(a.accountID),
		})
		if err != nil {
			return workflowView{}, err
		}
		return workflowView{completed: ws.Completed, state: string(ws.State), errMsg: ws.Error.Message}, nil
	}
	ws, err := a.client.Registrar.RegistrationStatus.Get(ctx, domainName, registrar.RegistrationStatusGetParams{
		AccountID: cloudflare.F(a.accountID),
	})
	if err != nil {
		return workflowView{}, err
	}
	return workflowView{completed: ws.Completed, state: string(ws.State), errMsg: ws.Error.Message}, nil
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
					"address book entry is used; without either, registration fails. In sandbox mode a built-in " +
					"placeholder contact is used when omitted.",
				Optional: true,
				Attributes: map[string]schema.Attribute{
					"email": &schema.StringAttribute{
						MarkdownDescription: "Email address of the registrant.",
						Required:            true,
					},
					"phone": &schema.StringAttribute{
						MarkdownDescription: "Phone number in E.164 format, e.g. `+1.5555555555`.",
						Required:            true,
						Validators: []validator.String{
							stringvalidator.RegexMatches(phoneNumberRegexp, "must be a phone number in E.164 format, e.g. `+1.5555555555` or `+15555555555`"),
						},
					},
					"fax": &schema.StringAttribute{
						MarkdownDescription: "Fax number in E.164 format. Optional.",
						Optional:            true,
						Validators: []validator.String{
							stringvalidator.RegexMatches(phoneNumberRegexp, "must be a fax number in E.164 format, e.g. `+1.5555555555` or `+15555555555`"),
						},
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
						Validators: []validator.String{
							stringvalidator.RegexMatches(countryCodeRegexp, "must be an ISO 3166-1 alpha-2 country code, e.g. `US`"),
						},
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

// Configure stores the shared cloudflare-go client; account_id and
// use_sandbox come from each resource instance.
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
	svc := r.registrarFor(accountID, plan.UseSandbox.ValueBool())
	domain := plan.Domain.ValueString()
	opts := createRegistrationOptions{
		domainName: domain,
		autoRenew:  plan.AutoRenew.ValueBool(),
		privacy:    plan.Privacy.ValueBool(),
		contact:    plan.RegistrantContact,
	}
	if !plan.PurchaseYears.IsNull() {
		opts.years = plan.PurchaseYears.ValueInt64()
	}
	// Sandbox registrations are simulations: fill a placeholder contact when
	// none is given so sandbox configs stay minimal. Production keeps the
	// account address book fallback and errors when neither is available.
	if opts.contact == nil && plan.UseSandbox.ValueBool() {
		opts.contact = sandboxPlaceholderContact()
	}
	// When relying on the account's default address book contact, tell the
	// user what to complete if Cloudflare rejects the registration.
	contactHint := ""
	if opts.contact == nil {
		contactHint = "; no registrant_contact was provided, so the account's default address book contact is used — complete it in the Cloudflare dashboard (Domain Registration → Registrant Contact) or add a registrant_contact block to this resource"
	}

	tflog.Info(ctx, fmt.Sprintf("Domain [%s] is registering...", domain))
	// Cloudflare permits one registration per domain, which makes the domain
	// name a natural idempotency key: adopt an existing registration (e.g.
	// one left behind by an apply that timed out but completed server-side)
	// instead of colliding with a second purchase attempt.
	if existing, err := svc.GetRegistration(ctx, domain); err == nil && existing.status != "" {
		tflog.Info(ctx, fmt.Sprintf("Domain [%s] is already registered (status: %s), adopting it", domain, existing.status))
		domainStateInto(ctx, resp, plan, existing)
		return
	}

	importID := fmt.Sprintf("%s/%s", accountID, domain)
	if plan.UseSandbox.ValueBool() {
		importID = "sandbox/" + importID
	}

	workflow, err := svc.CreateRegistration(ctx, opts)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed%s", domain, contactHint))
		return
	}

	if _, err := waitRegistrationWorkflow(ctx, svc, domain, workflow, domainRegistrationTimeout); err != nil {
		var timeoutErr *workflowTimeoutError
		if errors.As(err, &timeoutErr) {
			// A timeout is ambiguous: the workflow may still complete
			// server-side. Adopt the registration if it is already there,
			// otherwise point at import so the next apply cannot collide
			// with a second purchase attempt.
			if existing, gerr := svc.GetRegistration(ctx, domain); gerr == nil && existing.status != "" {
				tflog.Info(ctx, fmt.Sprintf("Domain [%s] completed after the workflow wait timed out (status: %s), adopting it", domain, existing.status))
				domainStateInto(ctx, resp, plan, existing)
				return
			}
			resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed%s; if the workflow completes later, import it instead of re-applying: terraform import st-cloudflare_domain.<name> %s", domain, contactHint, importID))
			return
		}
		resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed%s", domain, contactHint))
		return
	}

	registration, err := svc.GetRegistration(ctx, domain)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		return
	}
	tflog.Info(ctx, fmt.Sprintf("Domain [%s] registered, expires at [%s]", domain, registration.expiresAt))

	domainStateInto(ctx, resp, plan, registration)
}

// domainStateInto writes the created resource state from the plan and the
// registration the API reports. Plan values win for fields the API never
// returns or refuses to change (purchase_years, registrant_contact,
// privacy) so a managed resource always converges on its configuration.
func domainStateInto(ctx context.Context, resp *resource.CreateResponse, plan *cloudflareDomainModel, registration registrationView) {
	state := cloudflareDomainModel{
		AccountID:         plan.AccountID,
		UseSandbox:        plan.UseSandbox,
		Domain:            plan.Domain,
		PurchaseYears:     plan.PurchaseYears,
		AutoRenew:         plan.AutoRenew,
		Privacy:           plan.Privacy,
		RegistrantContact: plan.RegistrantContact,
		DomainExpiryDate:  types.StringValue(registration.expiresAt),
		Status:            types.StringValue(registration.status),
	}
	diags := resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
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
	svc := r.registrarFor(accountID, state.UseSandbox.ValueBool())
	domain := state.Domain.ValueString()
	registration, err := svc.GetRegistration(ctx, domain)
	if err != nil {
		var apiErr *cloudflare.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			resp.State.RemoveResource(ctx)
		} else {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		}
		return
	}

	state.AutoRenew = types.BoolValue(registration.autoRenew)
	if state.Privacy.IsNull() {
		// Privacy is chosen at purchase time and immutable; it is only
		// unknown directly after an import.
		state.Privacy = types.BoolValue(registration.privacy)
	}
	state.DomainExpiryDate = types.StringValue(registration.expiresAt)
	state.Status = types.StringValue(registration.status)

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

	accountID := plan.AccountID.ValueString()
	svc := r.registrarFor(accountID, plan.UseSandbox.ValueBool())
	domain := plan.Domain.ValueString()

	// The Registrar API supports updating auto_renew only; everything else is
	// set at registration time and cannot be changed afterwards. Fields the
	// API never returns (purchase_years, registrant_contact) stay null after
	// an import, so a null on either side means "unknown/unmanaged" and the
	// planned value is adopted rather than rejected as an update.
	changed := []string{}
	if !plan.PurchaseYears.Equal(state.PurchaseYears) && !plan.PurchaseYears.IsNull() && !state.PurchaseYears.IsNull() {
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
		workflow, err := svc.EditRegistration(ctx, domain, plan.AutoRenew.ValueBool())
		if err != nil {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "update domain [%s] failed", domain))
			return
		}
		if _, err := waitRegistrationWorkflow(ctx, svc, domain, workflow, domainUpdateTimeout); err != nil {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "update domain [%s] failed", domain))
			return
		}
	}

	registration, err := svc.GetRegistration(ctx, domain)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		return
	}

	newState := cloudflareDomainModel{
		AccountID:         plan.AccountID,
		UseSandbox:        plan.UseSandbox,
		Domain:            plan.Domain,
		PurchaseYears:     plan.PurchaseYears,
		AutoRenew:         types.BoolValue(registration.autoRenew),
		Privacy:           plan.Privacy,
		RegistrantContact: plan.RegistrantContact,
		DomainExpiryDate:  types.StringValue(registration.expiresAt),
		Status:            types.StringValue(registration.status),
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

	// Since a domain registration can not be deleted in Cloudflare, the best
	// we can do is stop it from renewing: when auto_renew is enabled, turn
	// it off so the registration simply lapses at the end of its term.
	domain := state.Domain.ValueString()
	if state.AutoRenew.ValueBool() {
		svc := r.registrarFor(state.AccountID.ValueString(), state.UseSandbox.ValueBool())
		workflow, err := svc.EditRegistration(ctx, domain, false)
		if err == nil {
			_, err = waitRegistrationWorkflow(ctx, svc, domain, workflow, domainUpdateTimeout)
		}
		if err != nil {
			resp.Diagnostics.AddWarning(
				"Failed to disable auto_renew",
				fmt.Sprintf("Destroying [%s] but disabling auto_renew failed: %s. The domain stays registered and may still renew — disable auto_renew manually to stop renewal charges.", domain, err),
			)
		}
	}
	msg := fmt.Sprintf("Since a domain registration can not be deleted in Cloudflare, [%s] remains registered until its term ends.", domain)
	tflog.Warn(ctx, msg)
}

// ImportState expects the ID in the format <account_id>/<domain>.
// Sandbox registrations use the format sandbox/<account_id>/<domain>.
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
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("domain"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("use_sandbox"), useSandbox)...)
}

// workflowTimeoutError marks a workflow wait that ran out of time; the
// workflow itself may still complete server-side.
type workflowTimeoutError struct {
	domain string
	state  string
	errMsg string
}

func (e *workflowTimeoutError) Error() string {
	msg := fmt.Sprintf("timed out waiting for registration workflow of [%s] (state=%s)", e.domain, e.state)
	if e.errMsg != "" {
		msg += ": " + e.errMsg
	}
	return msg
}

// waitRegistrationWorkflow polls the registration status until the workflow
// succeeds, reaches a state that needs user action, or the timeout is
// exhausted.
func waitRegistrationWorkflow(ctx context.Context, svc registrarService, domain string, workflow workflowView, timeout time.Duration) (workflowView, error) {
	if workflow.completed && workflow.state == string(registrar.WorkflowStatusStateSucceeded) {
		return workflow, nil
	}

	last := workflow
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for {
		w, err := svc.GetRegistrationStatus(ctx, domain)
		if err == nil {
			last = w
			switch w.state {
			case string(registrar.WorkflowStatusStateSucceeded):
				return w, nil
			case string(registrar.WorkflowStatusStateFailed), string(registrar.WorkflowStatusStateActionRequired), string(registrar.WorkflowStatusStateBlocked):
				hint := ""
				if w.state == string(registrar.WorkflowStatusStateBlocked) {
					hint = " — a third party such as the registry or a losing registrar is holding the workflow; if it resolves, re-applying adopts the existing registration"
				}
				return w, fmt.Errorf("registration workflow for [%s] state=%s: %s%s", domain, w.state, w.errMsg, hint)
			}
		}

		if time.Now().After(deadline) {
			return last, &workflowTimeoutError{domain: domain, state: last.state, errMsg: last.errMsg}
		}

		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(interval):
		}
		if interval < 30*time.Second {
			interval *= 2
		}
	}
}

var (
	// phoneNumberRegexp accepts strict E.164 (+15551234567) and Cloudflare's
	// dotted variant (+1.5551234567).
	phoneNumberRegexp = regexp.MustCompile(`^\+[1-9](\d{1,14}|\d{0,2}\.\d{1,14})$`)
	countryCodeRegexp = regexp.MustCompile(`^[A-Za-z]{2}$`)
)

// registrantContact is the plain-value form of the contact model shared by
// the production and sandbox adapters.
type registrantContact struct {
	email, phone, fax       string
	name, organization      string
	street, city, state     string
	postalCode, countryCode string
}

func contactOf(m *registrantContactModel) registrantContact {
	return registrantContact{
		email:        m.Email.ValueString(),
		phone:        m.Phone.ValueString(),
		fax:          m.Fax.ValueString(),
		name:         m.Name.ValueString(),
		organization: m.Organization.ValueString(),
		street:       m.Street.ValueString(),
		city:         m.City.ValueString(),
		state:        m.State.ValueString(),
		postalCode:   m.PostalCode.ValueString(),
		countryCode:  m.CountryCode.ValueString(),
	}
}

func privacyModeOf(privacy bool) string {
	if privacy {
		return "redaction"
	}
	return "off"
}

// sandboxPlaceholderContact returns a format-valid, obviously fake contact
// used when a sandbox registration omits registrant_contact.
func sandboxPlaceholderContact() *registrantContactModel {
	str := types.StringValue
	return &registrantContactModel{
		Email:       str("registrant@example.com"),
		Phone:       str("+1.5555555555"),
		Name:        str("Sandbox Registrant"),
		Street:      str("1 Sandbox Street"),
		City:        str("Austin"),
		State:       str("TX"),
		PostalCode:  str("78701"),
		CountryCode: str("US"),
	}
}

func registrationOf(autoRenew bool, privacyMode string, expiresAt time.Time, status string) registrationView {
	expiry := ""
	if !expiresAt.IsZero() {
		expiry = expiresAt.Format(time.RFC3339)
	}
	return registrationView{
		autoRenew: autoRenew,
		privacy:   privacyMode == "redaction",
		expiresAt: expiry,
		status:    status,
	}
}

func registrantContactEqual(a *registrantContactModel, b *registrantContactModel) bool {
	// A nil side is unknown/unmanaged (e.g. after an import, since the API
	// never returns contacts), not a difference — the caller adopts the
	// planned value.
	if a == nil || b == nil {
		return true
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
