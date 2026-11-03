package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/registrar"
	"github.com/cloudflare/cloudflare-go/v7/registrar_sandbox"
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
	Status           types.String  `tfsdk:"status"`
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
				MarkdownDescription: "Maximum price for the purchase. The provider checks the current registrar " +
					"price (first year plus any extra years at the renewal rate) and refuses to buy when it " +
					"exceeds this value.",
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
	useSandbox := plan.UseSandbox.ValueBool()
	svc := &registrarAdapter{client: r.client, accountID: accountID, sandbox: useSandbox}
	domain := plan.Domain.ValueString()

	// The registrant contact comes from the account's address book; when
	// the account has none, tell the user what to complete.
	contactHint := ""
	if !useSandbox {
		contactHint = "; if the account has no default address book contact, complete it in the Cloudflare dashboard (Domain Registration → Registrant Contact)"
	}

	years := plan.PurchaseYears.ValueInt64()
	if err := svc.ensurePriceWithin(ctx, domain, years, plan.MaxPrice.ValueFloat64()); err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed", domain))
		return
	}
	importID := fmt.Sprintf("%s/%s", accountID, domain)
	if useSandbox {
		importID = "sandbox/" + importID
	}
	tflog.Info(ctx, fmt.Sprintf("Domain [%s] is registering...", domain))
	createDomain := func() error {
		return svc.createDomain(ctx, domain, plan.AutoRenew.ValueBool(), plan.Privacy.ValueBool(), years)
	}
	if err := retry(ctx, createDomain); err != nil {
		// The purchase was rejected. When this account already holds the
		// registration, importing it beats a confusing not-available error;
		// otherwise let the unregistrable hint explain whose it is.
		if state, gerr := domainStateOf(ctx, svc, plan); gerr == nil && state.Status.ValueString() != "" {
			resp.Diagnostics.AddError(
				fmt.Sprintf("register domain [%s] failed", domain),
				fmt.Sprintf("%s\n\nThe domain is already registered under this account (status: %s); it was not adopted automatically. Import it into Terraform state instead: terraform import st-cloudflare_domain.<name> %s", err, state.Status.ValueString(), importID),
			)
			return
		}
		resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed%s%s", domain, contactHint, svc.unregistrableHint(ctx, domain)))
		return
	}
	if err := svc.waitWorkflow(ctx, domain, domainRegistrationTimeout); err != nil {
		var timeoutErr *workflowTimeoutError
		if errors.As(err, &timeoutErr) {
			// A timeout is ambiguous: the workflow may still complete
			// server-side. Adopt the registration if it is already there,
			// otherwise wait for the workflow to finish and import it.
			if state, gerr := domainStateOf(ctx, svc, plan); gerr == nil && state.Status.ValueString() != "" {
				tflog.Info(ctx, fmt.Sprintf("Domain [%s] completed after the workflow wait timed out (status: %s), adopting it", domain, state.Status.ValueString()))
				resp.Diagnostics.Append(resp.State.Set(ctx, state)...)
				return
			}
			resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed%s; if the workflow completes later, import it instead of re-applying: terraform import st-cloudflare_domain.<name> %s", domain, contactHint, importID))
			return
		}
		resp.Diagnostics.Append(diagnosticErrorOf(err, "register domain [%s] failed", domain))
		return
	}

	state, err := domainStateOf(ctx, svc, plan)
	if err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		return
	}
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
	svc := &registrarAdapter{client: r.client, accountID: accountID, sandbox: state.UseSandbox.ValueBool()}
	domain := state.Domain.ValueString()
	if err := svc.fillRegistration(ctx, domain, state); err != nil {
		var apiErr *cloudflare.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			resp.State.RemoveResource(ctx)
		} else {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		}
		return
	}

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
	svc := &registrarAdapter{client: r.client, accountID: accountID, sandbox: plan.UseSandbox.ValueBool()}
	domain := plan.Domain.ValueString()

	// The Registrar API supports updating auto_renew only; everything else is
	// set at registration time and cannot be changed afterwards. purchase_years
	// is never returned by the API, so a null on either side means
	// "unknown/unmanaged" and the planned value is adopted rather than
	// rejected as an update.
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
		if err := svc.setAutoRenew(ctx, domain, plan.AutoRenew.ValueBool(), domainUpdateTimeout); err != nil {
			resp.Diagnostics.Append(diagnosticErrorOf(err, "update domain [%s] failed", domain))
			return
		}
	}

	newState := cloudflareDomainModel{
		AccountID:     plan.AccountID,
		UseSandbox:    plan.UseSandbox,
		Domain:        plan.Domain,
		PurchaseYears: plan.PurchaseYears,
		MaxPrice:      plan.MaxPrice,
		Privacy:       plan.Privacy,
	}
	if err := svc.fillRegistration(ctx, domain, &newState); err != nil {
		resp.Diagnostics.Append(diagnosticErrorOf(err, "fetch registration for domain [%s] failed", domain))
		return
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
		svc := &registrarAdapter{client: r.client, accountID: state.AccountID.ValueString(), sandbox: state.UseSandbox.ValueBool()}
		if err := svc.setAutoRenew(ctx, domain, false, domainUpdateTimeout); err != nil {
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

// registrarAdapter talks to the Registrar API for one account: production
// endpoints, or the Registrar Sandbox endpoints when sandbox is set.
type registrarAdapter struct {
	client    *cloudflare.Client
	accountID string
	sandbox   bool
}

// createDomain purchases the domain (or simulates the purchase in the
// sandbox); retrying and waiting for the workflow are the caller's job.
func (a *registrarAdapter) createDomain(ctx context.Context, domainName string, autoRenew, privacy bool, years int64) error {
	if a.sandbox {
		params := registrar_sandbox.RegistrationNewParams{
			AccountID:   cloudflare.F(a.accountID),
			DomainName:  cloudflare.F(domainName),
			AutoRenew:   cloudflare.F(autoRenew),
			PrivacyMode: cloudflare.F(registrar_sandbox.RegistrationNewParamsPrivacyMode(privacyModeOf(privacy))),
			Contacts: cloudflare.F(registrar_sandbox.RegistrationNewParamsContacts{
				Registrant: cloudflare.F(registrar_sandbox.RegistrationNewParamsContactsRegistrant{
					Email: cloudflare.F("registrant@example.com"),
					Phone: cloudflare.F("+1.5555555555"),
					PostalInfo: cloudflare.F(registrar_sandbox.RegistrationNewParamsContactsRegistrantPostalInfo{
						Address: cloudflare.F(registrar_sandbox.RegistrationNewParamsContactsRegistrantPostalInfoAddress{
							Street:      cloudflare.F("1 Sandbox Street"),
							City:        cloudflare.F("Austin"),
							State:       cloudflare.F("TX"),
							PostalCode:  cloudflare.F("78701"),
							CountryCode: cloudflare.F("US"),
						}),
						Name: cloudflare.F("Sandbox Registrant"),
					}),
				}),
			}),
		}
		params.Years = cloudflare.F(years)
		_, err := a.client.RegistrarSandbox.Registrations.New(ctx, params)
		return err
	}
	params := registrar.RegistrationNewParams{
		AccountID:   cloudflare.F(a.accountID),
		DomainName:  cloudflare.F(domainName),
		AutoRenew:   cloudflare.F(autoRenew),
		PrivacyMode: cloudflare.F(registrar.RegistrationNewParamsPrivacyMode(privacyModeOf(privacy))),
	}
	params.Years = cloudflare.F(years)
	_, err := a.client.Registrar.Registrations.New(ctx, params)
	return err
}

// setAutoRenew flips auto_renew and waits for the workflow to finish.
func (a *registrarAdapter) setAutoRenew(ctx context.Context, domainName string, autoRenew bool, timeout time.Duration) error {
	if a.sandbox {
		_, err := a.client.RegistrarSandbox.Registrations.Edit(ctx, domainName, registrar_sandbox.RegistrationEditParams{
			AccountID: cloudflare.F(a.accountID),
			AutoRenew: cloudflare.F(autoRenew),
		})
		if err != nil {
			return err
		}
		return a.waitWorkflow(ctx, domainName, timeout)
	}
	_, err := a.client.Registrar.Registrations.Edit(ctx, domainName, registrar.RegistrationEditParams{
		AccountID: cloudflare.F(a.accountID),
		AutoRenew: cloudflare.F(autoRenew),
	})
	if err != nil {
		return err
	}
	return a.waitWorkflow(ctx, domainName, timeout)
}

// fillRegistration copies the registration the API reports into the state's
// computed fields. Privacy is filled only when unknown (directly after an
// import): it is chosen at purchase time and immutable, so a managed
// resource always keeps the value from its configuration.
func (a *registrarAdapter) fillRegistration(ctx context.Context, domainName string, state *cloudflareDomainModel) error {
	var autoRenew bool
	var privacyMode string
	var expiresAt time.Time
	var status string
	if a.sandbox {
		reg, err := a.client.RegistrarSandbox.Registrations.Get(ctx, domainName, registrar_sandbox.RegistrationGetParams{
			AccountID: cloudflare.F(a.accountID),
		})
		if err != nil {
			return err
		}
		autoRenew, privacyMode, expiresAt, status = reg.AutoRenew, string(reg.PrivacyMode), reg.ExpiresAt, string(reg.Status)
	} else {
		reg, err := a.client.Registrar.Registrations.Get(ctx, domainName, registrar.RegistrationGetParams{
			AccountID: cloudflare.F(a.accountID),
		})
		if err != nil {
			return err
		}
		autoRenew, privacyMode, expiresAt, status = reg.AutoRenew, string(reg.PrivacyMode), reg.ExpiresAt, string(reg.Status)
	}
	state.AutoRenew = types.BoolValue(autoRenew)
	if state.Privacy.IsNull() {
		state.Privacy = types.BoolValue(privacyMode == "redaction")
	}
	expiry := ""
	if !expiresAt.IsZero() {
		expiry = expiresAt.Format(time.RFC3339)
	}
	state.DomainExpiryDate = types.StringValue(expiry)
	state.Status = types.StringValue(status)
	return nil
}

// workflowStatus fetches the current registration workflow status once.
func (a *registrarAdapter) workflowStatus(ctx context.Context, domainName string) (completed bool, state, errMsg string, err error) {
	if a.sandbox {
		ws, err := a.client.RegistrarSandbox.RegistrationStatus.Get(ctx, domainName, registrar_sandbox.RegistrationStatusGetParams{
			AccountID: cloudflare.F(a.accountID),
		})
		if err != nil {
			return false, "", "", err
		}
		return ws.Completed, string(ws.State), ws.Error.Message, nil
	}
	ws, err := a.client.Registrar.RegistrationStatus.Get(ctx, domainName, registrar.RegistrationStatusGetParams{
		AccountID: cloudflare.F(a.accountID),
	})
	if err != nil {
		return false, "", "", err
	}
	return ws.Completed, string(ws.State), ws.Error.Message, nil
}

// waitWorkflow polls the workflow status until it succeeds, reaches a state
// that needs user action, or the timeout is exhausted.
func (a *registrarAdapter) waitWorkflow(ctx context.Context, domainName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second
	var state, errMsg string
	for {
		if _, wState, wErrMsg, err := a.workflowStatus(ctx, domainName); err == nil {
			state, errMsg = wState, wErrMsg
			switch state {
			case string(registrar.WorkflowStatusStateSucceeded):
				return nil
			case string(registrar.WorkflowStatusStateFailed), string(registrar.WorkflowStatusStateActionRequired), string(registrar.WorkflowStatusStateBlocked):
				hint := ""
				if state == string(registrar.WorkflowStatusStateBlocked) {
					hint = " — a third party such as the registry or a losing registrar is holding the workflow; if it later resolves, import the registration instead of re-applying"
				}
				return fmt.Errorf("registration workflow for [%s] state=%s: %s%s", domainName, state, errMsg, hint)
			}
		}
		if time.Now().After(deadline) {
			return &workflowTimeoutError{domain: domainName, state: state, errMsg: errMsg}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if interval < 30*time.Second {
			interval *= 2
		}
	}
}

// unregistrableHint runs the registrar domain-check and returns an
// explanation when the domain cannot be registered — e.g. it is already
// registered at the registry, premium priced, or on an unsupported
// extension. It returns an empty string when the domain is registrable or
// the check cannot run; Cloudflare's own error message already explains
// other failures.
func (a *registrarAdapter) unregistrableHint(ctx context.Context, domainName string) string {
	var registrable bool
	var reason string
	if a.sandbox {
		resp, err := a.client.RegistrarSandbox.Check(ctx, registrar_sandbox.RegistrarSandboxCheckParams{
			AccountID: cloudflare.F(a.accountID),
			Domains:   cloudflare.F([]string{domainName}),
		})
		if err != nil {
			return ""
		}
		for _, d := range resp.Domains {
			if d.Name == domainName {
				registrable, reason = d.Registrable, string(d.Reason)
			}
		}
	} else {
		resp, err := a.client.Registrar.Check(ctx, registrar.RegistrarCheckParams{
			AccountID: cloudflare.F(a.accountID),
			Domains:   cloudflare.F([]string{domainName}),
		})
		if err != nil {
			return ""
		}
		for _, d := range resp.Domains {
			if d.Name == domainName {
				registrable, reason = d.Registrable, string(d.Reason)
			}
		}
	}
	switch {
	case registrable:
		return ""
	case reason == "domain_unavailable":
		return "; domain-check reports the domain as already registered, reserved, or otherwise taken at the registry — it cannot be purchased (transfer it in or choose another name)"
	case reason == "domain_premium":
		return "; domain-check reports the domain as premium priced, which the Registrar API cannot register"
	case reason != "":
		return fmt.Sprintf("; domain-check reports the domain as not registrable (%s)", reason)
	}
	return "; domain-check reports the domain as not registrable"
}

// ensurePriceWithin refuses the purchase when the current registrar price
// exceeds maxPrice. The Registrar API has no server-side price cap, so
// max_price is enforced with a domain-check before purchasing: first year
// at the registration cost, each extra year at the renewal cost.
func (a *registrarAdapter) ensurePriceWithin(ctx context.Context, domainName string, years int64, maxPrice float64) error {
	var registrable, priced bool
	var currency, registrationCost, renewalCost string
	if a.sandbox {
		resp, err := a.client.RegistrarSandbox.Check(ctx, registrar_sandbox.RegistrarSandboxCheckParams{
			AccountID: cloudflare.F(a.accountID),
			Domains:   cloudflare.F([]string{domainName}),
		})
		if err != nil {
			return fmt.Errorf("price check for domain [%s] failed: %w", domainName, err)
		}
		for _, d := range resp.Domains {
			if d.Name == domainName {
				registrable, priced = d.Registrable, d.Pricing.RegistrationCost != ""
				currency, registrationCost, renewalCost = d.Pricing.Currency, d.Pricing.RegistrationCost, d.Pricing.RenewalCost
			}
		}
	} else {
		resp, err := a.client.Registrar.Check(ctx, registrar.RegistrarCheckParams{
			AccountID: cloudflare.F(a.accountID),
			Domains:   cloudflare.F([]string{domainName}),
		})
		if err != nil {
			return fmt.Errorf("price check for domain [%s] failed: %w", domainName, err)
		}
		for _, d := range resp.Domains {
			if d.Name == domainName {
				registrable, priced = d.Registrable, d.Pricing.RegistrationCost != ""
				currency, registrationCost, renewalCost = d.Pricing.Currency, d.Pricing.RegistrationCost, d.Pricing.RenewalCost
			}
		}
	}
	if !registrable {
		// The purchase itself will fail and explain why.
		return nil
	}
	if !priced {
		return fmt.Errorf("domain-check returned no pricing for domain [%s]; refusing to purchase because max_price cannot be verified", domainName)
	}
	first, err := strconv.ParseFloat(registrationCost, 64)
	if err != nil {
		return fmt.Errorf("domain-check returned an unparseable registration cost %q for domain [%s]", registrationCost, domainName)
	}
	renewal, err := strconv.ParseFloat(renewalCost, 64)
	if err != nil {
		return fmt.Errorf("domain-check returned an unparseable renewal cost %q for domain [%s]", renewalCost, domainName)
	}
	total := first
	if years > 1 {
		total += renewal * float64(years-1)
	}
	if total > maxPrice {
		breakdown := ""
		if years > 1 {
			breakdown = fmt.Sprintf(" (%.2f registration + %d x %.2f renewal)", first, years-1, renewal)
		}
		return fmt.Errorf("registration price for domain [%s] is %.2f %s%s, which exceeds max_price %.2f; refusing to purchase", domainName, total, currency, breakdown, maxPrice)
	}
	return nil
}

// domainStateOf builds the resource state from the plan plus the registration
// the API reports. Plan values win for fields the API never returns or
// refuses to change (purchase_years, privacy) so a managed resource always
// converges on its configuration.
func domainStateOf(ctx context.Context, svc *registrarAdapter, plan *cloudflareDomainModel) (*cloudflareDomainModel, error) {
	state := &cloudflareDomainModel{
		AccountID:     plan.AccountID,
		UseSandbox:    plan.UseSandbox,
		Domain:        plan.Domain,
		PurchaseYears: plan.PurchaseYears,
		MaxPrice:      plan.MaxPrice,
		AutoRenew:     plan.AutoRenew,
		Privacy:       plan.Privacy,
	}
	if err := svc.fillRegistration(ctx, plan.Domain.ValueString(), state); err != nil {
		return nil, err
	}
	// The framework requires Create's state to match the plan for
	// non-computed attributes; an adopted registration whose auto_renew
	// differs converges through Update on the next apply.
	state.AutoRenew = plan.AutoRenew
	return state, nil
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

func privacyModeOf(privacy bool) string {
	if privacy {
		return "redaction"
	}
	return "off"
}

// retry retries a failed call with exponential backoff, capping the total
// wait around 30s — the same behavior as the namecheap provider's
// createDomain retry.
func retry(ctx context.Context, f func() error) error {
	err := f()
	for delay := 500 * time.Millisecond; err != nil; delay *= 2 {
		if delay > 16*time.Second {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(delay):
		}
		err = f()
	}
	return nil
}
