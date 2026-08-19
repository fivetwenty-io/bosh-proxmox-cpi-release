package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/internal/constants"
	apierrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// maxErrorBodySnippet bounds how much of a non-2xx response body is read for
// error reporting. Error bodies from PVE are small JSON objects; the bound
// exists to protect against a misbehaving reverse proxy returning an
// oversized (or unbounded/streaming) HTML error page.
const maxErrorBodySnippet = 4096

// readBoundedBody reads up to maxErrorBodySnippet bytes from r. It is used
// only for building diagnostic error messages from non-2xx responses, never
// for decoding a successful response body.
func readBoundedBody(r io.Reader) []byte {
	body, _ := io.ReadAll(io.LimitReader(r, maxErrorBodySnippet))

	return body
}

var (
	ErrAuthenticationFailedNoTicket = errors.New("authentication failed: no ticket received")
	ErrLoginFailedNoTicket          = errors.New("login failed: no ticket received")
	ErrTFAFailedNoTicket            = errors.New("TFA failed: no ticket received")
)

// TicketAuthenticator provides ticket-based authentication for PVE.
// mu guards all reads and writes of the ticket field; Refresh/RefreshForce/
// SetTicket/CompleteTFA/processTFAResult acquire the write lock, GetTicket/
// GetHeaders/IsAuthenticated acquire the read lock.
type TicketAuthenticator struct {
	mu           sync.RWMutex
	baseURL      string
	httpClient   *http.Client
	credentials  *Credentials
	ticket       *Ticket
	cookieName   string
	pveNewFormat bool
}

// NewTicketAuthenticator creates a new ticket authenticator.
func NewTicketAuthenticator(
	baseURL string,
	credentials *Credentials,
	httpClient *http.Client,
	cookieName string,
	pveNewFormat bool,
) *TicketAuthenticator {
	if credentials.Realm == "" {
		credentials.Realm = realmPAM
	}

	return &TicketAuthenticator{
		baseURL:      baseURL,
		httpClient:   httpClient,
		credentials:  credentials,
		cookieName:   firstNonEmpty(cookieName, "PVEAuthCookie"),
		pveNewFormat: pveNewFormat,
	}
}

// NewTicketAuthenticatorFromTicket creates a ticket authenticator seeded with a
// pre-existing ticket value and CSRF token. This supports callers that already
// hold a valid PVE ticket (e.g. restored from storage) and want it used for
// authentication without performing a username/password login.
//
// The ticket's creation time is derived from the ticket value when it encodes a
// PVE timestamp; otherwise validity is anchored to now so proactive renewal can
// still occur. Credentials are retained (when non-nil) so the authenticator can
// renew the ticket using the ticket-as-password mechanism.
func NewTicketAuthenticatorFromTicket(
	baseURL string,
	ticketValue string,
	csrfToken string,
	username string,
	credentials *Credentials,
	httpClient *http.Client,
	cookieName string,
	pveNewFormat bool,
) *TicketAuthenticator {
	if credentials == nil {
		credentials = &Credentials{}
	}

	if credentials.Realm == "" {
		credentials.Realm = realmPAM
	}

	ticketAuth := &TicketAuthenticator{
		baseURL:      baseURL,
		httpClient:   httpClient,
		credentials:  credentials,
		cookieName:   firstNonEmpty(cookieName, "PVEAuthCookie"),
		pveNewFormat: pveNewFormat,
	}

	if ticketValue != "" {
		validUntil := time.Now().Add(constants.TicketValidity())

		createdAt, parseErr := ParseTicketTimestamp(ticketValue)
		if parseErr == nil {
			validUntil = createdAt.Add(constants.TicketValidity())
		}

		ticketAuth.ticket = &Ticket{
			Value:      ticketValue,
			CSRFToken:  csrfToken,
			Username:   username,
			ValidUntil: validUntil,
		}
	}

	return ticketAuth
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// Authenticate performs the authentication process.
func (ta *TicketAuthenticator) Authenticate() error {
	result, err := ta.login()
	if err != nil {
		return err
	}

	if result.TFAChallenge != nil {
		return &apierrors.TFARequiredError{
			Ticket:    result.TFAChallenge.Ticket,
			Challenge: result.TFAChallenge.Challenge,
			Types:     result.TFAChallenge.Types,
		}
	}

	if result.Ticket != nil {
		ta.mu.Lock()
		ta.ticket = result.Ticket
		ta.mu.Unlock()

		return nil
	}

	return ErrAuthenticationFailedNoTicket
}

// IsAuthenticated checks if the current session is authenticated.
func (ta *TicketAuthenticator) IsAuthenticated() bool {
	ta.mu.RLock()
	t := ta.ticket
	ta.mu.RUnlock()

	return t != nil && t.IsValid()
}

// GetHeaders returns the authentication headers.
func (ta *TicketAuthenticator) GetHeaders() map[string]string {
	ta.mu.RLock()
	tkt := ta.ticket
	ta.mu.RUnlock()

	if tkt == nil {
		return nil
	}

	headers := make(map[string]string)
	if tkt.Value != "" {
		headers["Cookie"] = fmt.Sprintf("%s=%s", ta.cookieName, tkt.Value)
	}

	if tkt.CSRFToken != "" {
		headers["CSRFPreventionToken"] = tkt.CSRFToken
	}

	return headers
}

// Refresh refreshes the authentication if necessary.
// If the ticket is valid, this is a no-op. If invalid, re-authenticates.
// For forced renewal (e.g., when ticket is old but not expired), use RefreshForce.
func (ta *TicketAuthenticator) Refresh() error {
	if ta.IsAuthenticated() {
		return nil
	}

	return ta.Authenticate()
}

// RefreshForce forces a ticket renewal regardless of current validity.
// This is useful for renewing tickets that are approaching expiration.
// Uses the ticket-as-password mechanism to renew using existing ticket.
func (ta *TicketAuthenticator) RefreshForce() error {
	result, err := ta.login()
	if err != nil {
		return fmt.Errorf("forced ticket renewal failed: %w", err)
	}

	if result.TFAChallenge != nil {
		return &apierrors.TFARequiredError{
			Ticket:    result.TFAChallenge.Ticket,
			Challenge: result.TFAChallenge.Challenge,
			Types:     result.TFAChallenge.Types,
		}
	}

	if result.Ticket != nil {
		ta.mu.Lock()
		ta.ticket = result.Ticket
		ta.mu.Unlock()

		return nil
	}

	return ErrAuthenticationFailedNoTicket
}

// Logout invalidates the ticket on the server and clears local authentication
// state. The local ticket is cleared only when the server confirms the
// logout with a 2xx response. On any other status (a permission error, a
// server error, or a proxy returning an HTML error page) the local ticket is
// left untouched and an error carrying the HTTP status and a bounded body
// snippet is returned, so the caller can inspect the failure and retry.
func (ta *TicketAuthenticator) Logout() error {
	ta.mu.RLock()
	hasTicket := ta.ticket != nil
	ta.mu.RUnlock()

	if !hasTicket {
		return nil
	}

	logoutURL := ta.baseURL + "/access/ticket"

	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, logoutURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create logout request: %w", err)
	}

	for key, value := range ta.GetHeaders() {
		req.Header.Set(key, value)
	}

	resp, err := ta.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send logout request: %w", err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body := readBoundedBody(resp.Body)

		return fmt.Errorf(
			"logout failed with status %d: %w", resp.StatusCode, apierrors.ParseAPIError(resp.StatusCode, body),
		)
	}

	ta.mu.Lock()
	ta.ticket = nil
	ta.mu.Unlock()

	return nil
}

// SetTicket sets the authentication ticket.
func (ta *TicketAuthenticator) SetTicket(ticket *Ticket) {
	ta.mu.Lock()
	ta.ticket = ticket
	ta.mu.Unlock()
}

// GetTicket returns the current authentication ticket.
func (ta *TicketAuthenticator) GetTicket() *Ticket {
	ta.mu.RLock()
	t := ta.ticket
	ta.mu.RUnlock()

	return t
}

type tfaResponse struct {
	Data struct {
		Ticket              string `json:"ticket"`
		CSRFPreventionToken string `json:"CSRFPreventionToken"`
		Username            string `json:"username"`
	} `json:"data"`
	Success int               `json:"success,omitempty"`
	Message string            `json:"message,omitempty"`
	Errors  map[string]string `json:"errors,omitempty"`
}

// CompleteTFA completes the two-factor authentication process.
func (ta *TicketAuthenticator) CompleteTFA(challenge *TFAChallenge, response *TFAResponse) (*AuthResult, error) {
	req, err := ta.createTFARequest(challenge, response)
	if err != nil {
		return nil, err
	}

	resp, err := ta.sendTFARequest(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	// Gate on status before decoding: a non-2xx response (e.g. a reverse
	// proxy returning an HTML 502/503 page) is not guaranteed to be the JSON
	// TFA envelope, so build the error directly from a bounded body snippet
	// instead of attempting to decode it.
	if resp.StatusCode != http.StatusOK {
		body := readBoundedBody(resp.Body)

		return &AuthResult{
			Success: false,
			Error: fmt.Errorf(
				"TFA request failed with status %d: %w", resp.StatusCode, apierrors.ParseAPIError(resp.StatusCode, body),
			),
		}, nil
	}

	tfaResp, err := ta.parseTFAResponse(resp)
	if err != nil {
		return nil, err
	}

	return ta.processTFAResult(resp, tfaResp), nil
}

func (ta *TicketAuthenticator) createTFARequest(challenge *TFAChallenge, response *TFAResponse) (*http.Request, error) {
	tfaURL := ta.baseURL + "/access/tfa"

	data := url.Values{}
	data.Set("response", response.Response)

	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		tfaURL,
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create TFA request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	if challenge.Ticket != "" {
		req.Header.Set("Cookie", fmt.Sprintf("%s=%s", ta.cookieName, challenge.Ticket))
	}

	return req, nil
}

func (ta *TicketAuthenticator) sendTFARequest(req *http.Request) (*http.Response, error) {
	resp, err := ta.httpClient.Do(req)
	if err != nil {
		return nil, &apierrors.ConnectionError{
			Message: "TFA request failed",
			Cause:   err,
		}
	}

	return resp, nil
}

// parseTFAResponse decodes the TFA JSON envelope. Callers must only invoke
// this for a 2xx response; CompleteTFA gates on status before calling it.
func (ta *TicketAuthenticator) parseTFAResponse(resp *http.Response) (*tfaResponse, error) {
	var tfaResp tfaResponse

	decoder := json.NewDecoder(resp.Body)

	err := decoder.Decode(&tfaResp)
	if err != nil {
		return nil, fmt.Errorf("failed to parse TFA response: %w", err)
	}

	return &tfaResp, nil
}

func (ta *TicketAuthenticator) processTFAResult(resp *http.Response, tfaResp *tfaResponse) *AuthResult {
	if resp.StatusCode != http.StatusOK || tfaResp.Success != 1 {
		return &AuthResult{
			Success: false,
			Error:   apierrors.ParseAPIError(resp.StatusCode, []byte(tfaResp.Message)),
		}
	}

	if tfaResp.Data.Ticket != "" {
		validUntil := time.Now().Add(constants.TicketValidity())

		ticket := &Ticket{
			Value:      tfaResp.Data.Ticket,
			CSRFToken:  tfaResp.Data.CSRFPreventionToken,
			Username:   tfaResp.Data.Username,
			ValidUntil: validUntil,
		}

		ta.mu.Lock()
		ta.ticket = ticket
		ta.mu.Unlock()

		return &AuthResult{
			Success: true,
			Ticket:  ticket,
		}
	}

	return &AuthResult{
		Success: false,
		Error:   ErrTFAFailedNoTicket,
	}
}

func (ta *TicketAuthenticator) login() (*AuthResult, error) {
	loginURL := ta.baseURL + "/access/ticket"
	data := ta.prepareLoginData()

	req, err := ta.createLoginRequest(loginURL, data)
	if err != nil {
		return nil, err
	}

	resp, err := ta.sendLoginRequest(req)
	if err != nil {
		return nil, err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	return ta.processLoginResponse(resp)
}

func (ta *TicketAuthenticator) prepareLoginData() url.Values {
	data := url.Values{}
	data.Set("username", fmt.Sprintf("%s@%s", ta.credentials.Username, ta.credentials.Realm))

	// Use password, or fallback to ticket if password is empty.
	// PVE API allows using an existing ticket as password to renew/create a new ticket.
	password := ta.credentials.Password

	ta.mu.RLock()
	tkt := ta.ticket
	ta.mu.RUnlock()

	if password == "" && tkt != nil && tkt.Value != "" {
		password = tkt.Value
	}

	data.Set("password", password)

	if ta.credentials.OTP != "" {
		data.Set("otp", ta.credentials.OTP)
	}

	if ta.pveNewFormat {
		data.Set("new-format", "1")
	}

	return data
}

func (ta *TicketAuthenticator) createLoginRequest(loginURL string, data url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		loginURL,
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create login request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return req, nil
}

func (ta *TicketAuthenticator) sendLoginRequest(req *http.Request) (*http.Response, error) {
	resp, err := ta.httpClient.Do(req)
	if err != nil {
		return nil, &apierrors.ConnectionError{
			Message: "login request failed",
			Cause:   err,
		}
	}

	return resp, nil
}

// loginResponse is the JSON envelope PVE returns from POST /access/ticket,
// covering both a successful login and a TFA challenge.
type loginResponse struct {
	Data struct {
		Ticket              string                 `json:"ticket"`
		CSRFPreventionToken string                 `json:"CSRFPreventionToken"`
		Username            string                 `json:"username"`
		Cap                 map[string]interface{} `json:"cap"`
		// TFA fields
		NeedTFA   bool     `json:"NeedTFA,omitempty"`
		Ticket2   string   `json:"ticket2,omitempty"`
		Challenge string   `json:"challenge,omitempty"`
		TFATypes  []string `json:"tfa-types,omitempty"`
	} `json:"data"`
	Success int               `json:"success,omitempty"`
	Message string            `json:"message,omitempty"`
	Errors  map[string]string `json:"errors,omitempty"`
}

// processLoginResponse decodes the login JSON envelope. It gates on the HTTP
// status before decoding: a non-2xx response (e.g. a reverse proxy returning
// an HTML 502/503 page) is not guaranteed to be the JSON login envelope, so
// the error is built directly from a bounded body snippet instead of
// attempting — and failing — to decode it as JSON.
func (ta *TicketAuthenticator) processLoginResponse(resp *http.Response) (*AuthResult, error) {
	if resp.StatusCode != http.StatusOK {
		body := readBoundedBody(resp.Body)

		return &AuthResult{
			Success: false,
			Error: fmt.Errorf(
				"login failed with status %d: %w", resp.StatusCode, apierrors.ParseAPIError(resp.StatusCode, body),
			),
		}, nil
	}

	var response loginResponse

	decoder := json.NewDecoder(resp.Body)

	err := decoder.Decode(&response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse login response: %w", err)
	}

	return buildLoginResult(&response), nil
}

// buildLoginResult classifies a decoded 2xx login response into a TFA
// challenge, a successful ticket, or a no-ticket failure.
func buildLoginResult(response *loginResponse) *AuthResult {
	if response.Data.NeedTFA || response.Data.Ticket2 != "" {
		return &AuthResult{
			Success: false,
			TFAChallenge: &TFAChallenge{
				Ticket:    response.Data.Ticket2,
				Challenge: response.Data.Challenge,
				Types:     response.Data.TFATypes,
			},
		}
	}

	if response.Data.Ticket != "" {
		validUntil := time.Now().Add(constants.TicketValidity())

		return &AuthResult{
			Success: true,
			Ticket: &Ticket{
				Value:      response.Data.Ticket,
				CSRFToken:  response.Data.CSRFPreventionToken,
				Username:   response.Data.Username,
				ValidUntil: validUntil,
			},
		}
	}

	return &AuthResult{
		Success: false,
		Error:   ErrLoginFailedNoTicket,
	}
}
