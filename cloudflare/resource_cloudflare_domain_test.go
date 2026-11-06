package cloudflare_test

import (
	"fmt"
	"os"
	"testing"

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
				Config:                  testAccDomainResource(false),
				ImportState:             true,
				ImportStateId:           fmt.Sprintf("sandbox/%s/%s", os.Getenv("CLOUDFLARE_ACCOUNT_ID"), os.Getenv("CLOUDFLARE_DOMAIN_TEST")),
				ResourceName:            "st-cloudflare_domain.test",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"purchase_years", "max_price"},
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
