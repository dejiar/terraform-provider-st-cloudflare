# The name of Terraform custom provider.
CUSTOM_PROVIDER_NAME ?= terraform-provider-st-cloudflare
# The url of Terraform provider.
CUSTOM_PROVIDER_URL ?= example.local/myklst/st-cloudflare

.PHONY: install-local-custom-provider
install-local-custom-provider:
	@export PROVIDER_LOCAL_PATH='$(CUSTOM_PROVIDER_URL)'; \
	PLUGIN_DIR="$$HOME/.terraform.d/plugins/$(CUSTOM_PROVIDER_URL)/0.1.0/linux_amd64"; \
	mkdir -p "$$PLUGIN_DIR"; \
	go build -o "$$PLUGIN_DIR/$(CUSTOM_PROVIDER_NAME)" .; \
	unset PROVIDER_LOCAL_PATH
