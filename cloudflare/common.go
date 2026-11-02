package cloudflare

import (
	"context"
	"os"
	"regexp"
	"strconv"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure the implementation satisfies the expected interfaces
var (
	_ provider.Provider = &cloudflareProvider{}
)

type cloudflareProvider struct{}

// providerClients bundles the shared cloudflare-go client and the provider
// settings handed to resources and data sources. The domain resource uses
// accountID and useSandbox to pick the Registrar or Registrar Sandbox
// service.
type providerClients struct {
	cloudflare *cloudflare.Client
	accountID  string
	useSandbox bool
}

type cloudflareProviderModel struct {
	Email      types.String `tfsdk:"email" json:"email"`
	APIKey     types.String `tfsdk:"api_key" json:"api_key"`
	APIToken   types.String `tfsdk:"api_token" json:"api_token"`
	AccountID  types.String `tfsdk:"account_id" json:"account_id"`
	UseSandbox types.Bool   `tfsdk:"use_sandbox" json:"use_sandbox"`
}

// New is a helper function to simplify provider server
func New() provider.Provider {
	return &cloudflareProvider{}
}

// Metadata returns the provider type name.
func (p *cloudflareProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "st-cloudflare"
}

// Schema defines the provider-level schema for configuration data.
func (p *cloudflareProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "The Cloudflare provider is used to interact with the Cloudflare to zones from it. " +
			"The provider needs to be configured with the proper credentials before it can be used.",
		Attributes: map[string]schema.Attribute{
			"email": schema.StringAttribute{
				Description: "A registered Cloudflare email address. May also be provided via CLOUDFLARE_EMAIL " +
					"environment variable. Required when using `api_key`. Conflicts with `api_token`.",
				Optional:   true,
				Validators: []validator.String{},
			},
			"api_key": schema.StringAttribute{
				Description: "The API key for operations. May also be provided via CLOUDFLARE_API_KEY environment variable. " +
					"API keys are now considered legacy by Cloudflare, API tokens should be used instead. " +
					"Must provide only one of `api_key`, `api_token`.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(
						regexp.MustCompile(`[0-9a-f]{37}`),
						"API key must be 37 characters long and only contain characters 0-9 and a-f (all lowercased)",
					),
					stringvalidator.AlsoRequires(path.Expressions{
						path.MatchRoot("email"),
					}...),
				},
			},
			"api_token": schema.StringAttribute{
				Description: "The API Token for operations. May also be provided via CLOUDFLARE_API_TOKEN " +
					"environment variable. Must provide only one of `api_key`, `api_token`.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(
						regexp.MustCompile(`[A-Za-z0-9-_]{40}`),
						"API tokens must be 40 characters long and only contain characters a-z, A-Z, 0-9, hyphens and underscores",
					),
				},
			},
			"account_id": schema.StringAttribute{
				Description: "The Cloudflare account ID for Registrar operations. May also be provided via " +
					"CLOUDFLARE_ACCOUNT_ID environment variable. Required by the `st-cloudflare_domain` resource.",
				Optional: true,
			},
			"use_sandbox": schema.BoolAttribute{
				Description: "Whether to use the Registrar Sandbox endpoints for domain registrations, which " +
					"allow free test registrations (`.com`/`.net` only) without real domains or billing. " +
					"May also be provided via CLOUDFLARE_USE_SANDBOX environment variable.",
				Optional: true,
			},
		},
	}
}

func (p *cloudflareProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config cloudflareProviderModel

	diags := req.Config.Get(ctx, &config)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Default values to environment variables, but override
	// with Terraform configuration value if set.
	var email, apiKey, apiToken, accountID string
	if !config.Email.IsNull() {
		email = config.Email.ValueString()
	} else {
		email = os.Getenv("CLOUDFLARE_EMAIL")
	}

	if !config.APIKey.IsNull() {
		apiKey = config.APIKey.ValueString()
	} else {
		apiKey = os.Getenv("CLOUDFLARE_API_KEY")
	}

	if !config.APIToken.IsNull() {
		apiToken = config.APIToken.ValueString()
	} else {
		apiToken = os.Getenv("CLOUDFLARE_API_TOKEN")
	}

	if !config.AccountID.IsNull() {
		accountID = config.AccountID.ValueString()
	} else {
		accountID = os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	}

	var useSandbox bool
	if !config.UseSandbox.IsNull() {
		useSandbox = config.UseSandbox.ValueBool()
	} else if v := os.Getenv("CLOUDFLARE_USE_SANDBOX"); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			resp.Diagnostics.AddAttributeError(
				path.Root("use_sandbox"),
				"Invalid Cloudflare use_sandbox",
				"The provider cannot create the Registrar client as there is an invalid value for the "+
					"CLOUDFLARE_USE_SANDBOX environment variable. Set the use_sandbox value in the configuration "+
					"or fix the environment variable to a valid boolean.",
			)
			return
		}
		useSandbox = parsed
	}

	// API Token or both email and API Key have to be set, return
	// errors with provider-specific guidance.
	if apiToken == "" && (email == "" || apiKey == "") {
		if email == "" {
			resp.Diagnostics.AddAttributeError(
				path.Root("email"),
				"Missing Cloudflare Email",
				"Provide either an API token or both email and API key. You can also use CLOUDFLARE_EMAIL and CLOUDFLARE_API_KEY environment variables.",
			)
		}
		if apiKey == "" {
			resp.Diagnostics.AddAttributeError(
				path.Root("api_key"),
				"Missing Cloudflare API Key",
				"Provide either an API token or both email and API key. You can also use CLOUDFLARE_EMAIL and CLOUDFLARE_API_KEY environment variables.",
			)
		}
		if apiToken == "" {
			resp.Diagnostics.AddAttributeError(
				path.Root("api_token"),
				"Missing Cloudflare API Token",
				"Provide an API token or both email and API key. You can also use CLOUDFLARE_API_TOKEN environment variable.",
			)
		}

		if resp.Diagnostics.HasError() {
			return
		}
	}

	// Initialize client using API token if provided, else use email and API key.
	var client *cloudflare.Client
	if apiToken != "" {
		client = cloudflare.NewClient(
			option.WithAPIToken(apiToken),
		)
	} else {
		client = cloudflare.NewClient(
			option.WithAPIKey(apiKey),
			option.WithAPIEmail(email),
		)
	}

	clients := &providerClients{
		cloudflare: client,
		accountID:  accountID,
		useSandbox: useSandbox,
	}

	resp.DataSourceData = clients
	resp.ResourceData = clients
}

func (p *cloudflareProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

func (p *cloudflareProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewZoneTypeResource,
		NewCloudflareDomainResource,
	}
}
