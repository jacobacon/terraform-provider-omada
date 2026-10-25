// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ControllerInfo is the subset of GET /api/info we need. omadacId prefixes
// every subsequent /api/v2 path.
type ControllerInfo struct {
	OmadacID      string `json:"omadacId"`
	ControllerVer string `json:"controllerVer"`
	APIVer        string `json:"apiVer"`
	Type          int    `json:"type"` // 0 = software, 1 = OC200, 2 = OC300, ...
}

// loginResult is the payload of a successful login: the CSRF token.
type loginResult struct {
	Token string `json:"token"`
}

// mfaChallenge is the payload the controller returns *instead of* a token when
// the account is subject to two-factor authentication.
//
// Both fields are optional, and on a 6.1.0.19 OC200 the whole result is `{}`:
// the login page reads `MFAId` and `supportedMFATypes` defensively and falls
// back to an empty id and an authenticator-app code. Verified live — an empty
// `MFAId` is accepted by checkMFACodeAndLogin and returns a session token.
type mfaChallenge struct {
	MFAId             string `json:"MFAId"`
	SupportedMFATypes []int  `json:"supportedMFATypes"`
}

// The controller's 2FA error codes, as handled by its own login page:
//
//	-30165  2FA is required for this local user
//	-30138  2FA is required (same handling in the UI; seen on other builds)
//	-30139  the submitted code was wrong. `result.codeRemainAttempts` counts
//	        down to a temporary account lock, so a retry loop is not safe.
//
// mfaTypeEmail/mfaTypeTOTP are the `mfaType` values the login page sends: a
// code mailed to the account, or one from an authenticator app. Only TOTP can
// be automated, so only TOTP is implemented here.
const (
	mfaTypeEmail = 2
	mfaTypeTOTP  = 3
)

func isMFARequired(code int) bool {
	return code == -30165 || code == -30138
}

// mfaRejection is the payload accompanying -30139. `codeRemainAttempts` is the
// controller's own countdown to locking the account, and is what makes a
// bounded retry safe rather than reckless.
type mfaRejection struct {
	CodeRemainAttempts *int `json:"codeRemainAttempts"`
	LockedMinutes      *int `json:"lockedMinutes"`
}

// mfaMinRemainingAttempts is the floor for retrying a rejected code. Codes are
// single-use, and a *separate process* — the previous terraform command, a
// browser login — may have already spent the one for this window, which looks
// identical to a wrong secret. One retry in the next window rescues that case;
// stopping while attempts remain keeps a genuinely wrong secret from walking
// the account into a lock.
const mfaMinRemainingAttempts = 3

// mfaGuidance ends every "cannot get past 2FA" error. Both ways out are
// deliberate operator choices, so name both rather than implying the only fix
// is to weaken the controller.
const mfaGuidance = "Either enrol this account with an authenticator app and set the provider's `totp_secret` " +
	"(or OMADA_TOTP_SECRET) to the secret behind its QR code, or turn off the controller-wide requirement at " +
	"Global View -> Settings -> Account Security."

// info fetches controller metadata (no auth required).
func (c *Client) info(ctx context.Context) (*ControllerInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/info", nil)
	if err != nil {
		return nil, fmt.Errorf("building info request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /api/info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading info response: %w", err)
	}

	var env APIResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decoding info envelope (http %d): %w", resp.StatusCode, err)
	}
	if env.ErrorCode != 0 {
		return nil, &APIError{Code: env.ErrorCode, Msg: env.Msg}
	}

	var out ControllerInfo
	if err := json.Unmarshal(env.Result, &out); err != nil {
		return nil, fmt.Errorf("decoding controller info: %w", err)
	}
	if out.OmadacID == "" {
		return nil, fmt.Errorf("controller info returned an empty omadacId")
	}
	return &out, nil
}

// login performs the info + login handshake and stores omadacId + CSRF token.
// It holds the login mutex so concurrent callers don't stampede the controller.
func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	inf, err := c.info(ctx)
	if err != nil {
		return err
	}
	c.omadacID = inf.OmadacID

	env, err := c.postAuthJSON(ctx, "/api/v2/login", map[string]string{
		"username": c.username,
		"password": c.password,
	})
	if err != nil {
		return err
	}

	// A controller with 2FA enforced answers the password with a challenge
	// rather than a token, and the session only exists after it is answered.
	if isMFARequired(env.ErrorCode) {
		env, err = c.answerMFAChallenge(ctx, env)
		if err != nil {
			return err
		}
	}

	if env.ErrorCode != 0 {
		return fmt.Errorf("login failed: %w", &APIError{Code: env.ErrorCode, Msg: env.Msg})
	}

	var lr loginResult
	if err := json.Unmarshal(env.Result, &lr); err != nil {
		return fmt.Errorf("decoding login result: %w", err)
	}
	if lr.Token == "" {
		return fmt.Errorf("login succeeded but returned an empty token")
	}
	c.token = lr.Token
	return nil
}

// postAuthJSON posts a JSON body to one of the unauthenticated /api/v2 login
// endpoints and returns the decoded envelope. A non-zero errorCode is handed
// back rather than turned into an error, because the login exchange gives
// several of them specific meanings.
func (c *Client) postAuthJSON(ctx context.Context, path string, payload any) (*APIResponse, error) {
	endpoint := fmt.Sprintf("%s/%s%s", c.baseURL, c.omadacID, path)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshalling %s body: %w", path, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("building %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", path, err)
	}

	var env APIResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decoding %s envelope (http %d): %w", path, resp.StatusCode, err)
	}
	return &env, nil
}

// answerMFAChallenge completes a login the controller stopped for two-factor
// authentication, by sending the current authenticator-app code back with the
// MFAId that identifies the half-finished login. This is the same exchange the
// controller's login page performs.
func (c *Client) answerMFAChallenge(ctx context.Context, env *APIResponse) (*APIResponse, error) {
	challengeErr := &APIError{Code: env.ErrorCode, Msg: env.Msg}

	if c.totp == nil {
		return nil, fmt.Errorf("login failed: %w. This account is subject to two-factor authentication and no TOTP secret is configured. %s",
			challengeErr, mfaGuidance)
	}

	var challenge mfaChallenge
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &challenge); err != nil {
			return nil, fmt.Errorf("decoding 2FA challenge: %w", err)
		}
	}
	if len(challenge.SupportedMFATypes) > 0 && !slices.Contains(challenge.SupportedMFATypes, mfaTypeTOTP) {
		names := make([]string, 0, len(challenge.SupportedMFATypes))
		for _, t := range challenge.SupportedMFATypes {
			names = append(names, mfaTypeName(t))
		}
		return nil, fmt.Errorf("login failed: %w. This account's 2FA methods (%s) do not include an authenticator app, and only authenticator-app codes can be generated without a human. %s",
			challengeErr, strings.Join(names, ", "), mfaGuidance)
	}

	// At most two attempts, and the second only in a later code window — see
	// mfaMinRemainingAttempts.
	for attempt := range 2 {
		code, err := c.nextTOTPCode(ctx)
		if err != nil {
			return nil, err
		}

		// MFAId is echoed back as-is, empty included: that is what the login
		// page sends when the challenge omitted it, which is live 6.1 behaviour.
		resp, err := c.postAuthJSON(ctx, "/api/v2/checkMFACodeAndLogin", map[string]any{
			"username": c.username,
			"password": c.password,
			"code":     code,
			"MFAId":    challenge.MFAId,
			"mfaType":  mfaTypeTOTP,
		})
		if err != nil {
			return nil, err
		}

		// Spent either way: the controller has now seen this code.
		c.lastTOTPCode = code

		if resp.ErrorCode != -30139 {
			return resp, nil
		}

		var rejection mfaRejection
		if len(resp.Result) > 0 {
			_ = json.Unmarshal(resp.Result, &rejection)
		}

		// The controller has already locked the account: a retry would just
		// draw another lock error, so stop here rather than spend the second
		// attempt against it.
		if rejection.LockedMinutes != nil {
			return nil, fmt.Errorf("login failed: %w. The controller has locked this account for %d minute(s) after too many rejected codes. Check that the TOTP secret belongs to this account and that this machine's clock is accurate",
				&APIError{Code: resp.ErrorCode, Msg: resp.Msg}, *rejection.LockedMinutes)
		}

		remaining := "an unknown number of"
		if rejection.CodeRemainAttempts != nil {
			remaining = fmt.Sprintf("%d", *rejection.CodeRemainAttempts)
		}

		outOfBudget := rejection.CodeRemainAttempts != nil && *rejection.CodeRemainAttempts < mfaMinRemainingAttempts
		if attempt == 1 || outOfBudget {
			return nil, fmt.Errorf("login failed: %w. The controller rejected the generated code, with %s attempts left before it locks the account. Check that the TOTP secret belongs to this account and that this machine's clock is accurate",
				&APIError{Code: resp.ErrorCode, Msg: resp.Msg}, remaining)
		}
	}

	// Unreachable: the loop either returns a response or an error.
	return nil, fmt.Errorf("login failed: exhausted 2FA attempts without a verdict")
}

// nextTOTPCode returns a code that has not been sent before. A re-login after a
// session timeout can fall in the same 30-second window as the previous one,
// and a controller is within its rights to refuse a replayed code — better to
// wait out the window than to spend one of the account's few attempts.
func (c *Client) nextTOTPCode(ctx context.Context) (string, error) {
	now := time.Now()
	if code := c.totp.codeAt(now); code != c.lastTOTPCode {
		return code, nil
	}

	timer := time.NewTimer(time.Until(c.totp.nextWindow(now)))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for a fresh 2FA code: %w", ctx.Err())
	case <-timer.C:
	}
	return c.totp.codeAt(time.Now()), nil
}

// mfaTypeName renders a controller mfaType for an error message.
func mfaTypeName(t int) string {
	switch t {
	case mfaTypeEmail:
		return "email"
	case mfaTypeTOTP:
		return "authenticator app"
	default:
		return fmt.Sprintf("type %d", t)
	}
}

// ControllerVersion returns cached controller metadata (fetched at login).
func (c *Client) ControllerInfo(ctx context.Context) (*ControllerInfo, error) {
	return c.info(ctx)
}

// isV5 reports whether the controller runs a 5.x release. A few writes need a
// different body or route there than on v6.
func (c *Client) isV5(ctx context.Context) (bool, error) {
	inf, err := c.ControllerInfo(ctx)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(inf.ControllerVer, "5."), nil
}
