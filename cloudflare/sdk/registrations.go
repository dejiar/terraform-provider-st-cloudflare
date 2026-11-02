package sdk

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Registration workflow states.
const (
	WorkflowStateSucceeded      = "succeeded"
	WorkflowStateFailed         = "failed"
	WorkflowStateActionRequired = "action_required"
)

type RegistrationContactAddress struct {
	Street      string `json:"street"`
	City        string `json:"city"`
	State       string `json:"state,omitempty"`
	PostalCode  string `json:"postal_code"`
	CountryCode string `json:"country_code"`
}

type RegistrationContactPostalInfo struct {
	Address      RegistrationContactAddress `json:"address"`
	Name         string                     `json:"name"`
	Organization string                     `json:"organization,omitempty"`
}

// RegistrationContact describes a registrant contact. Phone numbers use
// E.164 format (`+{country_code}.{number}`).
type RegistrationContact struct {
	Email      string                        `json:"email"`
	Phone      string                        `json:"phone"`
	Fax        string                        `json:"fax,omitempty"`
	PostalInfo RegistrationContactPostalInfo `json:"postal_info"`
}

// RegistrationContacts carries the contact roles accepted at registration.
// Every currently supported extension requires only the registrant; when it
// is omitted the account's default address book entry is used instead.
type RegistrationContacts struct {
	Registrant *RegistrationContact `json:"registrant,omitempty"`
}

type RegistrationCreateParams struct {
	DomainName  string                `json:"domain_name"`
	AutoRenew   bool                  `json:"auto_renew"`
	PrivacyMode string                `json:"privacy_mode,omitempty"`
	Years       int64                 `json:"years,omitempty"`
	Contacts    *RegistrationContacts `json:"contacts,omitempty"`
}

// Registration is a domain registration resource.
type Registration struct {
	DomainName  string `json:"domain_name"`
	AutoRenew   bool   `json:"auto_renew"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
	Locked      bool   `json:"locked"`
	PrivacyMode string `json:"privacy_mode"`
	Status      string `json:"status"`
}

type RegistrationWorkflowError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RegistrationWorkflow tracks an async registration or update workflow.
type RegistrationWorkflow struct {
	Completed bool   `json:"completed"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	State     string `json:"state"`
	Links     struct {
		Self     string `json:"self"`
		Resource string `json:"resource"`
	} `json:"links"`
	Context struct {
		DomainName   string        `json:"domain_name"`
		Registration *Registration `json:"registration"`
	} `json:"context"`
	Error *RegistrationWorkflowError `json:"error"`
}

func (w *RegistrationWorkflow) errorMessage() string {
	if w.Error != nil {
		return w.Error.Message
	}
	return ""
}

// RegistrationsCreate starts a domain registration workflow.
func (c *Client) RegistrationsCreate(ctx context.Context, params RegistrationCreateParams) (*RegistrationWorkflow, error) {
	var workflow RegistrationWorkflow
	if err := c.do(ctx, http.MethodPost, c.registrarPath("registrations"), params, &workflow); err != nil {
		return nil, err
	}
	return &workflow, nil
}

// RegistrationsGet returns the current state of a domain registration.
func (c *Client) RegistrationsGet(ctx context.Context, domainName string) (*Registration, error) {
	var registration Registration
	if err := c.do(ctx, http.MethodGet, c.registrarPath("registrations", domainName), nil, &registration); err != nil {
		return nil, err
	}
	return &registration, nil
}

// RegistrationsEdit updates a domain registration. The Registrar API
// currently supports updating auto_renew only.
func (c *Client) RegistrationsEdit(ctx context.Context, domainName string, autoRenew bool) (*RegistrationWorkflow, error) {
	var workflow RegistrationWorkflow
	body := map[string]bool{"auto_renew": autoRenew}
	if err := c.do(ctx, http.MethodPatch, c.registrarPath("registrations", domainName), body, &workflow); err != nil {
		return nil, err
	}
	return &workflow, nil
}

// RegistrationStatus returns the current status of a registration workflow
// via its links.self URL.
func (c *Client) RegistrationStatus(ctx context.Context, selfLink string) (*RegistrationWorkflow, error) {
	var workflow RegistrationWorkflow
	if err := c.do(ctx, http.MethodGet, selfLink, nil, &workflow); err != nil {
		return nil, err
	}
	return &workflow, nil
}

// WaitRegistrationWorkflow polls links.self until the workflow succeeds,
// reaches a state that needs user action, or the timeout is exhausted.
func (c *Client) WaitRegistrationWorkflow(ctx context.Context, workflow *RegistrationWorkflow, timeout time.Duration) (*RegistrationWorkflow, error) {
	if workflow.Completed {
		return workflow, nil
	}

	last := workflow
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for {
		w, err := c.RegistrationStatus(ctx, last.Links.Self)
		if err == nil {
			last = w
			switch w.State {
			case WorkflowStateSucceeded:
				return w, nil
			case WorkflowStateFailed, WorkflowStateActionRequired:
				return w, fmt.Errorf("registration workflow for [%s] state=%s: %s", w.Context.DomainName, w.State, w.errorMessage())
			}
		}

		if time.Now().After(deadline) {
			return last, fmt.Errorf("timed out waiting for registration workflow of [%s] (state=%s)", last.Context.DomainName, last.State)
		}

		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(interval):
		}
		if interval < 30*time.Second {
			interval *= 2
		}
	}
}
