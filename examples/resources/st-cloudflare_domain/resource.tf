resource "st-cloudflare_domain" "example_domain" {
  account_id     = "023e105f4ecef8ad9ca31a8372d0c353"
  domain         = "example.com"
  purchase_years = 1
  max_price      = 20
  auto_renew     = true
  privacy        = true
}
