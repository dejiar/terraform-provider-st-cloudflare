terraform {
  required_providers {
    st-cloudflare = {
      source  = "myklst/st-cloudflare"
      version = "~> 0.1"
    }
  }
}

provider "st-cloudflare" {
  # Preferred: API token, e.g. a Registrar-scoped token.
  # May also be provided via the CLOUDFLARE_API_TOKEN environment variable.
  api_token = "xxx"

  # Legacy alternative: Global API key + account email.
  # May also be provided via CLOUDFLARE_API_KEY / CLOUDFLARE_EMAIL.
  # api_key = "xxx"
  # email   = "user@example.com"
}
