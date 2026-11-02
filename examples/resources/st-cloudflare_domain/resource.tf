resource "st-cloudflare_domain" "example_domain" {
  account_id = "023e105f4ecef8ad9ca31a8372d0c353"
  domain     = "example.com"
  auto_renew = true
  privacy    = true

  registrant_contact = {
    email        = "admin@example.com"
    phone        = "+1.5555555555"
    name         = "Ada Lovelace"
    street       = "1 Example Street"
    city         = "Austin"
    state        = "TX"
    postal_code  = "78701"
    country_code = "US"
  }
}
