package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Privacy modes accepted by the Registrar API.
const (
	PrivacyModeOff       = "off"
	PrivacyModeRedaction = "redaction"
)

const apiBaseURL = "https://api.cloudflare.com/client/v4"

// Client is a minimal REST client for the Cloudflare Registrar API.
// When UseSandbox is true, requests target the registrar-sandbox endpoints,
// which allow registration testing without real domains or billing.
type Client struct {
	APIToken   string
	AccountID  string
	UseSandbox bool
	HTTPClient *http.Client
}

// NewClient creates a Registrar API client bound to an account.
func NewClient(apiToken string, accountID string, useSandbox bool) *Client {
	return &Client{
		APIToken:   apiToken,
		AccountID:  accountID,
		UseSandbox: useSandbox,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) registrarPath(elements ...string) string {
	registrar := "registrar"
	if c.UseSandbox {
		registrar = "registrar-sandbox"
	}
	return strings.Join(append([]string{"accounts", c.AccountID, registrar}, elements...), "/")
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiResponse struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

func (c *Client) do(ctx context.Context, method string, path string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, apiBaseURL+"/"+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var envelope apiResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("unexpected registrar api response (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if !envelope.Success {
		if len(envelope.Errors) > 0 {
			e := envelope.Errors[0]
			return fmt.Errorf("registrar api error %d: %s", e.Code, e.Message)
		}
		return fmt.Errorf("registrar api error (HTTP %d)", resp.StatusCode)
	}
	if out != nil && len(envelope.Result) > 0 {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}
