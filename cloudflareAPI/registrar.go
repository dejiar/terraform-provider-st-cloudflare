// Package cloudflareapi talks to the Cloudflare Registrar API: production
// endpoints, or the Registrar Sandbox endpoints when sandbox is set.
package cloudflareapi

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/registrar"
	"github.com/cloudflare/cloudflare-go/v7/registrar_sandbox"
)

// NewRegistrar returns a Registrar bound to one Cloudflare account.
func NewRegistrar(client *cloudflare.Client, accountID string, sandbox bool) *Registrar {
	return &Registrar{client: client, accountID: accountID, sandbox: sandbox}
}

// Registrar talks to the Registrar API for one account: production
// endpoints, or the Registrar Sandbox endpoints when sandbox is set.
type Registrar struct {
	client    *cloudflare.Client
	accountID string
	sandbox   bool
}

// createDomain checks the first-year price against maxPrice and purchases
// the domain (or simulates the purchase in the sandbox); retrying and
// waiting for the workflow are the caller's job.
func (a *Registrar) CreateDomain(ctx context.Context, domainName string, autoRenew, privacy bool, years int64, maxPrice float64) error {
	// The Registrar API is case-insensitive and normalizes names to
	// lowercase; do the same on every call so domain-check responses
	// match by name and GET/PATCH paths resolve.
	domainName = strings.ToLower(domainName)

	if err := a.CheckDomainPrice(ctx, domainName, maxPrice); err != nil {
		return err
	}
	privacyMode := "off"
	if privacy {
		privacyMode = "redaction"
	}
	if a.sandbox {
		params := registrar_sandbox.RegistrationNewParams{
			AccountID:   cloudflare.F(a.accountID),
			DomainName:  cloudflare.F(domainName),
			AutoRenew:   cloudflare.F(autoRenew),
			PrivacyMode: cloudflare.F(registrar_sandbox.RegistrationNewParamsPrivacyMode(privacyMode)),
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
		PrivacyMode: cloudflare.F(registrar.RegistrationNewParamsPrivacyMode(privacyMode)),
	}
	params.Years = cloudflare.F(years)
	_, err := a.client.Registrar.Registrations.New(ctx, params)
	return err
}

// SetAutoRenew flips auto_renew and waits for the update workflow to finish.
// The Edit response carries the initial workflow state; the update-status
// endpoint tracks that workflow, so wait for it there — registration-status
// only reflects the original purchase.
func (a *Registrar) SetAutoRenew(ctx context.Context, domainName string, autoRenew bool, timeout time.Duration) error {
	// The Registrar API is case-insensitive and normalizes names to
	// lowercase; do the same on every call so domain-check responses
	// match by name and GET/PATCH paths resolve.
	domainName = strings.ToLower(domainName)

	if a.sandbox {
		ws, err := a.client.RegistrarSandbox.Registrations.Edit(ctx, domainName, registrar_sandbox.RegistrationEditParams{
			AccountID: cloudflare.F(a.accountID),
			AutoRenew: cloudflare.F(autoRenew),
		})
		if err != nil {
			return err
		}
		return a.pollWorkflow(ctx, domainName, ws.Completed, string(ws.State), ws.Error.Message, timeout, "update", func() (bool, string, string, error) {
			ws, err := a.client.RegistrarSandbox.UpdateStatus.Get(ctx, domainName, registrar_sandbox.UpdateStatusGetParams{
				AccountID: cloudflare.F(a.accountID),
			})
			if err != nil {
				return false, "", "", err
			}
			return ws.Completed, string(ws.State), ws.Error.Message, nil
		})
	}
	ws, err := a.client.Registrar.Registrations.Edit(ctx, domainName, registrar.RegistrationEditParams{
		AccountID: cloudflare.F(a.accountID),
		AutoRenew: cloudflare.F(autoRenew),
	})
	if err != nil {
		return err
	}
	return a.pollWorkflow(ctx, domainName, ws.Completed, string(ws.State), ws.Error.Message, timeout, "update", func() (bool, string, string, error) {
		ws, err := a.client.Registrar.UpdateStatus.Get(ctx, domainName, registrar.UpdateStatusGetParams{
			AccountID: cloudflare.F(a.accountID),
		})
		if err != nil {
			return false, "", "", err
		}
		return ws.Completed, string(ws.State), ws.Error.Message, nil
	})
}

// pollWorkflow polls an async Registrar workflow until it reaches a terminal
// state or the timeout expires. seed* carries the status the triggering call
// already returned (an Edit response has one; a purchase does not) and fetch
// polls the endpoint that tracks this workflow: registration-status for
// purchases, update-status for edits. subject names the workflow in error
// messages. A failed status fetch just leaves the previous state and tries
// again on the next tick.
func (a *Registrar) pollWorkflow(ctx context.Context, domainName string, seedCompleted bool, seedState, seedErrMsg string, timeout time.Duration, subject string, fetch func() (completed bool, state, errMsg string, err error)) error {
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second
	completed, state, errMsg := seedCompleted, seedState, seedErrMsg
	for {
		switch state {
		case string(registrar.WorkflowStatusStateSucceeded):
			return nil
		case string(registrar.WorkflowStatusStateFailed), string(registrar.WorkflowStatusStateActionRequired), string(registrar.WorkflowStatusStateBlocked):
			hint := ""
			if state == string(registrar.WorkflowStatusStateBlocked) {
				hint = " — a third party such as the registry or a losing registrar is holding the workflow; if it later resolves, import the registration instead of re-applying"
			}
			return fmt.Errorf("%s workflow for [%s] state=%s: %s%s", subject, domainName, state, errMsg, hint)
		}
		if completed {
			return fmt.Errorf("%s workflow for [%s] ended in state=%s: %s", subject, domainName, state, errMsg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s workflow for [%s] did not finish within %s (last state: %s)", subject, domainName, timeout, state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
		if interval < 30*time.Second {
			interval *= 2
		}
		if c, s, e, err := fetch(); err == nil {
			completed, state, errMsg = c, s, e
		}
	}
}

// getDomainInfo fetches the registration the API reports and returns its
// auto_renew flag, privacy mode, and expiry date.
func (a *Registrar) GetDomainInfo(ctx context.Context, domainName string) (autoRenew bool, privacy string, expiresAt string, err error) {
	// The Registrar API is case-insensitive and normalizes names to
	// lowercase; do the same on every call so domain-check responses
	// match by name and GET/PATCH paths resolve.
	domainName = strings.ToLower(domainName)

	if a.sandbox {
		reg, err := a.client.RegistrarSandbox.Registrations.Get(ctx, domainName, registrar_sandbox.RegistrationGetParams{
			AccountID: cloudflare.F(a.accountID),
		})
		if err != nil {
			return false, "", "", err
		}
		return reg.AutoRenew, string(reg.PrivacyMode), reg.ExpiresAt.Format(time.RFC3339), nil
	}
	reg, err := a.client.Registrar.Registrations.Get(ctx, domainName, registrar.RegistrationGetParams{
		AccountID: cloudflare.F(a.accountID),
	})
	if err != nil {
		return false, "", "", err
	}
	return reg.AutoRenew, string(reg.PrivacyMode), reg.ExpiresAt.Format(time.RFC3339), nil
}

// waitWorkflow polls the registration (purchase) workflow until it succeeds,
// reaches a state that needs user action, or the timeout is exhausted.
func (a *Registrar) WaitWorkflow(ctx context.Context, domainName string, timeout time.Duration) error {
	// The Registrar API is case-insensitive and normalizes names to
	// lowercase; do the same on every call so domain-check responses
	// match by name and GET/PATCH paths resolve.
	domainName = strings.ToLower(domainName)

	if a.sandbox {
		return a.pollWorkflow(ctx, domainName, false, "", "", timeout, "registration", func() (bool, string, string, error) {
			ws, err := a.client.RegistrarSandbox.RegistrationStatus.Get(ctx, domainName, registrar_sandbox.RegistrationStatusGetParams{
				AccountID: cloudflare.F(a.accountID),
			})
			if err != nil {
				return false, "", "", err
			}
			return ws.Completed, string(ws.State), ws.Error.Message, nil
		})
	}
	return a.pollWorkflow(ctx, domainName, false, "", "", timeout, "registration", func() (bool, string, string, error) {
		ws, err := a.client.Registrar.RegistrationStatus.Get(ctx, domainName, registrar.RegistrationStatusGetParams{
			AccountID: cloudflare.F(a.accountID),
		})
		if err != nil {
			return false, "", "", err
		}
		return ws.Completed, string(ws.State), ws.Error.Message, nil
	})
}

// checkDomainPrice refuses the purchase when the current first-year
// registrar price exceeds maxPrice.
// The Registrar API has no server-side price cap, so the check runs on the
// domain-check pricing before purchasing.
func (a *Registrar) CheckDomainPrice(ctx context.Context, domainName string, maxPrice float64) error {
	// The Registrar API is case-insensitive and normalizes names to
	// lowercase; do the same on every call so domain-check responses
	// match by name and GET/PATCH paths resolve.
	domainName = strings.ToLower(domainName)

	var registrable, priced bool
	var currency, registrationCost string
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
				currency, registrationCost = d.Pricing.Currency, d.Pricing.RegistrationCost
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
				currency, registrationCost = d.Pricing.Currency, d.Pricing.RegistrationCost
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
	if first > maxPrice {
		return fmt.Errorf("registration price for domain [%s] is %.2f %s, which exceeds max_price %.2f; refusing to purchase", domainName, first, currency, maxPrice)
	}
	return nil
}
