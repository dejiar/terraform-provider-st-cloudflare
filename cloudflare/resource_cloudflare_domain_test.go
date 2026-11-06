package cloudflare_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/myklst/terraform-provider-st-cloudflare/cloudflare"
)

func testAccPreCheck(t *testing.T) {
	t.Helper()
	for _, env := range []string{"CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID", "CLOUDFLARE_DOMAIN_TEST"} {
		if os.Getenv(env) == "" {
			t.Fatalf("%s must be set for acceptance tests", env)
		}
	}
}

func testAccProtoV6ProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"st-cloudflare": providerserver.NewProtocol6WithError(cloudflare.New()),
	}
}

func testAccDomainResource(autoRenew bool) string {
	return fmt.Sprintf(`
resource "st-cloudflare_domain" "test" {
  account_id     = %q
  use_sandbox    = true
  domain         = %q
  purchase_years = 1
  max_price      = 12.0
  auto_renew     = %t
  privacy        = true
}
`, os.Getenv("CLOUDFLARE_ACCOUNT_ID"), os.Getenv("CLOUDFLARE_DOMAIN_TEST"), autoRenew)
}

// TestAccCloudflareDomainLifecycle walks the full resource lifecycle against
// the Cloudflare Registrar Sandbox: create, import, update, delete.
func TestAccCloudflareDomainLifecycle(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			// CREATE: register the domain in the sandbox.
			{
				Config: testAccDomainResource(false),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "domain", os.Getenv("CLOUDFLARE_DOMAIN_TEST")),
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "use_sandbox", "true"),
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "auto_renew", "false"),
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "privacy", "true"),
					resource.TestCheckResourceAttrSet("st-cloudflare_domain.test", "domain_expiry_date"),
				),
			},
			// IMPORT: sandbox/<account_id>/<domain>, state verified against
			// the create step (purchase_years and max_price are not part of
			// the import ID and stay unset until a config refills them).
			{
				Config:                               testAccDomainResource(false),
				ImportState:                          true,
				ImportStateId:                        fmt.Sprintf("sandbox/%s/%s", os.Getenv("CLOUDFLARE_ACCOUNT_ID"), os.Getenv("CLOUDFLARE_DOMAIN_TEST")),
				ResourceName:                         "st-cloudflare_domain.test",
				ImportStateVerify:                    true,
				ImportStateVerifyIgnore:              []string{"purchase_years", "max_price"},
				ImportStateVerifyIdentifierAttribute: "domain",
			},
			// UPDATE: flip auto_renew on.
			{
				Config: testAccDomainResource(true),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "auto_renew", "true"),
				),
			},
			// DELETE: dropping the resource from the config runs Delete,
			// which turns auto_renew back off (a registration cannot be
			// deleted, so it is left to lapse).
			{
				Config: "# delete step: resource removed from config\n",
			},
		},
	})
}

// TestAccCloudflareDomainRenewalLifecycle purchases with auto_renew enabled,
// disables it, re-enables it, and destroys — covering both edit directions
// plus the destroy-time disable.
func TestAccCloudflareDomainRenewalLifecycle(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			// CREATE with auto_renew on.
			{
				Config: testAccDomainResource(true),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "auto_renew", "true"),
					resource.TestCheckResourceAttrSet("st-cloudflare_domain.test", "domain_expiry_date"),
				),
			},
			// UPDATE: disable auto_renew.
			{
				Config: testAccDomainResource(false),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "auto_renew", "false"),
				),
			},
			// UPDATE: re-enable auto_renew. The Registrar Sandbox refuses this
			// with 422 "Domain must be registered" — an enable is allowed only
			// once per sandbox registration (purchasing with auto_renew=true
			// counts as that one enable), because the sandbox has no billing
			// and cannot authorize renewal charges. Production has no such
			// restriction.
			{
				Config:      testAccDomainResource(true),
				ExpectError: regexp.MustCompile(`Domain\s+must\s+be\s+registered`),
			},
			// DELETE: destroy flips auto_renew back off.
			{
				Config: "# delete step: resource removed from config\n",
			},
		},
	})
}

func testAccMixedCaseDomainResource(maxPrice float64) string {
	return fmt.Sprintf(`
resource "st-cloudflare_domain" "test" {
  account_id     = %q
  use_sandbox    = true
  domain         = %q
  purchase_years = 1
  max_price      = %.1f
  auto_renew     = false
  privacy        = true
}
`, os.Getenv("CLOUDFLARE_ACCOUNT_ID"), strings.ToUpper(os.Getenv("CLOUDFLARE_DOMAIN_MIXED_TEST")), maxPrice)
}

// TestAccCloudflareDomainMixedCaseNormalization proves the provider lowercases
// the domain at the API boundary: a mixed-case config still hits the
// max_price guard (step 1), purchases cleanly (step 2, state keeps the
// configured case), and survives a refresh without the 404 ghost (step 3).
func TestAccCloudflareDomainMixedCaseNormalization(t *testing.T) {
	if os.Getenv("CLOUDFLARE_DOMAIN_MIXED_TEST") == "" {
		t.Skip("CLOUDFLARE_DOMAIN_MIXED_TEST must be set for the mixed-case test")
	}
	mixed := strings.ToUpper(os.Getenv("CLOUDFLARE_DOMAIN_MIXED_TEST"))
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			// max_price below the real price: must refuse even though the
			// config domain is mixed-case (the old code skipped the check).
			{
				Config:      testAccMixedCaseDomainResource(1.0),
				ExpectError: regexp.MustCompile(`exceeds\s+max_price`),
			},
			// Purchase with a sane cap: succeeds because the API calls are
			// lowercased; state keeps the configured (mixed) case.
			{
				Config: testAccMixedCaseDomainResource(12.0),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "domain", mixed),
					resource.TestCheckResourceAttrSet("st-cloudflare_domain.test", "domain_expiry_date"),
				),
			},
			// Refresh: if the read 404'd on the mixed-case name, the plan
			// would try to re-purchase and fail as already registered.
			{
				Config: testAccMixedCaseDomainResource(12.0),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("st-cloudflare_domain.test", "domain", mixed),
				),
			},
		},
	})
}

// TestAccCloudflareDomainMultiYearPriceGuard proves max_price compares the
// whole-term cost: 2 years at the sandbox .com price (registration + renewal)
// total roughly double the first year, so a cap above the first-year price
// alone must still refuse.
func TestAccCloudflareDomainMultiYearPriceGuard(t *testing.T) {
	domain := fmt.Sprintf("stcf-my-%06d.com", time.Now().UnixNano()%1000000)
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "st-cloudflare_domain" "test" {
  account_id     = %q
  use_sandbox    = true
  domain         = %q
  purchase_years = 2
  max_price      = 15.0
  auto_renew     = false
  privacy        = true
}
`, os.Getenv("CLOUDFLARE_ACCOUNT_ID"), domain),
				ExpectError: regexp.MustCompile(`exceeds\s+max_price`),
			},
		},
	})
}
