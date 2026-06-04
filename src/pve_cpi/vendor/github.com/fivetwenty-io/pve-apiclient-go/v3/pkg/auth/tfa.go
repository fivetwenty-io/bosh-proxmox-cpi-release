package auth

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
	"golang.org/x/term"
)

const realmPAM = "pam"

var (
	ErrHardwareTokenRequiresBrowser = errors.New("hardware token authentication (U2F/WebAuthn) requires browser interaction")
	ErrNoTFAResponseConfigured      = errors.New("no TFA response configured for available types")
)

// TFAType represents the type of two-factor authentication.
type TFAType string

const (
	// TFATypeTOTP represents Time-based One-Time Password (e.g., Google Authenticator).
	TFATypeTOTP TFAType = "totp"
	// TFATypeYubico represents Yubico OTP.
	TFATypeYubico TFAType = "yubico"
	// TFATypeRecovery represents recovery codes.
	TFATypeRecovery TFAType = "recovery"
	// TFATypeU2F represents Universal 2nd Factor.
	TFATypeU2F TFAType = "u2f"
	// TFATypeWebAuthn represents WebAuthn/FIDO2.
	TFATypeWebAuthn TFAType = "webauthn"
)

// TFAHandler handles two-factor authentication interactions.
type TFAHandler interface {
	// HandleTFAChallenge handles a TFA challenge and returns the response.
	HandleTFAChallenge(challenge *TFAChallenge) (*TFAResponse, error)
}

// InteractiveTFAHandler provides interactive TFA handling via terminal.
type InteractiveTFAHandler struct {
	reader *bufio.Reader
}

// NewInteractiveTFAHandler creates a new interactive TFA handler.
func NewInteractiveTFAHandler() *InteractiveTFAHandler {
	return &InteractiveTFAHandler{
		reader: bufio.NewReader(os.Stdin),
	}
}

// HandleTFAChallenge interactively handles a TFA challenge.
func (h *InteractiveTFAHandler) HandleTFAChallenge(challenge *TFAChallenge) (*TFAResponse, error) {
	// Display available TFA types
	_, _ = fmt.Fprintf(os.Stderr, "Two-factor authentication required.\n")

	if len(challenge.Types) > 0 {
		_, _ = fmt.Fprintf(os.Stderr, "Available TFA types: %s\n", strings.Join(challenge.Types, ", "))
	}

	// Determine which type to use
	tfaType := h.selectTFAType(challenge.Types)

	// Get the TFA response based on type
	var (
		response string
		err      error
	)

	switch TFAType(tfaType) {
	case TFATypeTOTP:
		response, err = h.promptTOTP()
	case TFATypeYubico:
		response, err = h.promptYubico()
	case TFATypeRecovery:
		response, err = h.promptRecovery()
	case TFATypeU2F, TFATypeWebAuthn:
		return nil, ErrHardwareTokenRequiresBrowser
	default:
		response, err = h.promptGeneric(tfaType)
	}

	if err != nil {
		return nil, err
	}

	return &TFAResponse{
		Response: response,
		Type:     tfaType,
	}, nil
}

// selectTFAType allows the user to select a TFA type.
func (h *InteractiveTFAHandler) selectTFAType(availableTypes []string) string {
	if len(availableTypes) == 0 {
		// Default to TOTP if no types specified
		return string(TFATypeTOTP)
	}

	if len(availableTypes) == 1 {
		// Only one type available
		return availableTypes[0]
	}

	// Multiple types available, let user choose
	_, _ = fmt.Fprintln(os.Stderr, "Select TFA type:")

	for i, t := range availableTypes {
		_, _ = fmt.Fprintf(os.Stderr, "%d. %s\n", i+1, t)
	}

	for {
		_, _ = fmt.Fprint(os.Stderr, "Enter choice (1-", len(availableTypes), "): ")

		input, err := h.reader.ReadString('\n')
		if err != nil {
			continue
		}

		input = strings.TrimSpace(input)

		// Check if user entered a number
		var choice int

		_, err = fmt.Sscanf(input, "%d", &choice)
		if err == nil {
			if choice >= 1 && choice <= len(availableTypes) {
				return availableTypes[choice-1]
			}
		}

		// Check if user entered the type name directly
		for _, t := range availableTypes {
			if strings.EqualFold(input, t) {
				return t
			}
		}

		_, _ = fmt.Fprintln(os.Stderr, "Invalid choice. Please try again.")
	}
}

// promptTOTP prompts for a TOTP code.
func (h *InteractiveTFAHandler) promptTOTP() (string, error) {
	_, _ = fmt.Fprint(os.Stderr, "Enter TOTP code: ")

	code, err := h.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read TOTP code: %w", err)
	}

	return strings.TrimSpace(code), nil
}

// promptYubico prompts for a Yubico OTP.
func (h *InteractiveTFAHandler) promptYubico() (string, error) {
	_, _ = fmt.Fprint(os.Stderr, "Touch your YubiKey or enter Yubico OTP: ")

	otp, err := h.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read Yubico OTP: %w", err)
	}

	return strings.TrimSpace(otp), nil
}

// promptRecovery prompts for a recovery code.
func (h *InteractiveTFAHandler) promptRecovery() (string, error) {
	_, _ = fmt.Fprint(os.Stderr, "Enter recovery code: ")

	code, err := h.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read recovery code: %w", err)
	}

	return strings.TrimSpace(code), nil
}

// promptGeneric prompts for a generic TFA response.
func (h *InteractiveTFAHandler) promptGeneric(tfaType string) (string, error) {
	_, _ = fmt.Fprintf(os.Stderr, "Enter %s code: ", tfaType)

	code, err := h.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read %s code: %w", tfaType, err)
	}

	return strings.TrimSpace(code), nil
}

// AutoTFAHandler automatically handles TFA with predefined responses.
type AutoTFAHandler struct {
	responses map[TFAType]string
}

// NewAutoTFAHandler creates a new automatic TFA handler.
func NewAutoTFAHandler(responses map[TFAType]string) *AutoTFAHandler {
	return &AutoTFAHandler{
		responses: responses,
	}
}

// HandleTFAChallenge automatically handles a TFA challenge.
func (h *AutoTFAHandler) HandleTFAChallenge(challenge *TFAChallenge) (*TFAResponse, error) {
	// Try to find a matching response
	for _, availableType := range challenge.Types {
		if response, ok := h.responses[TFAType(availableType)]; ok {
			return &TFAResponse{
				Response: response,
				Type:     availableType,
			}, nil
		}
	}

	// Try default TOTP if no types specified
	if len(challenge.Types) == 0 {
		if response, ok := h.responses[TFATypeTOTP]; ok {
			return &TFAResponse{
				Response: response,
				Type:     string(TFATypeTOTP),
			}, nil
		}
	}

	return nil, fmt.Errorf("%w: %v", ErrNoTFAResponseConfigured, challenge.Types)
}

// PromptPassword prompts for a password without echoing to terminal.
func PromptPassword(prompt string) (string, error) {
	_, _ = fmt.Fprint(os.Stderr, prompt)

	// Read password without echo. Convert to int explicitly: on Windows
	// syscall.Stdin is a syscall.Handle (uintptr), not an int, so the bare
	// value fails to cross-compile for windows/*.
	password, err := term.ReadPassword(int(syscall.Stdin))

	_, _ = fmt.Fprintln(os.Stderr) // Print newline after password input

	if err != nil {
		return "", fmt.Errorf("failed to read password: %w", err)
	}

	return string(password), nil
}

// PromptUsername prompts for a username.
func PromptUsername(prompt string) (string, error) {
	_, _ = fmt.Fprint(os.Stderr, prompt)

	reader := bufio.NewReader(os.Stdin)

	username, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read username: %w", err)
	}

	return strings.TrimSpace(username), nil
}

// PromptCredentials prompts for username and password.
func PromptCredentials() (*Credentials, error) {
	username, err := PromptUsername("Username: ")
	if err != nil {
		return nil, err
	}

	password, err := PromptPassword("Password: ")
	if err != nil {
		return nil, err
	}

	// Parse realm from username if present (user@realm format)
	realm := realmPAM // default realm

	if parts := strings.Split(username, "@"); len(parts) == constants.ExpectedPartsCount {
		username = parts[0]
		realm = parts[1]
	}

	return &Credentials{
		Username: username,
		Password: password,
		Realm:    realm,
	}, nil
}
