package cloudflare

import (
	"context"
	"os"
	"regexp"

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

// providerClients bundles the shared cloudflare-go client handed to
// resources and data sources. Resources carry their own scope (account,
// sandbox) as resource attributes.
type providerClients struct {
	cloudflare *cloudflare.Client
}

type cloudflareProviderModel struct {
	Email    types.String `tfsdk:"email" json:"email"`
	APIKey   types.String `tfsdk:"api_key" json:"api_key"`
	APIToken types.String `tfsdk:"api_token" json:"api_token"`
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
				Optional:  true,
				Sensitive: true,
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
				Optional:  true,
				Sensitive: true,
				Validators: []validator.String{
					stringvalidator.RegexMatches(
						regexp.MustCompile(`^(cfat_)?[A-Za-z0-9_-]{40,}$`),
						"API tokens must be at least 40 characters (or a cfat_ account-owned token) and only contain characters a-z, A-Z, 0-9, hyphens and underscores",
					),
				},
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
	var email, apiKey, apiToken string
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
