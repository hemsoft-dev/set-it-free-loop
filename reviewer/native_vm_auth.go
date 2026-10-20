package reviewer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// nativeAuthValues recognizes only the pinned clients' subscription-file
// formats. It does not establish entitlement, policy support or activation.
// Returned canary copies belong only to the privileged controller, never a
// source reader, publisher or code-host child. Caller clears those copies.
func nativeAuthValues(provider Provider, raw []byte) ([][]byte, error) {
	if !provider.Valid() || len(raw) == 0 || len(raw) > authTransferLimit || uniqueJSONKeys(json.NewDecoder(bytes.NewReader(raw)), 0) != nil {
		return nil, ErrAuthTransfer
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return nil, ErrAuthTransfer
	}
	var values [][]byte
	add := func(value string) bool {
		if len(value) == 0 || len(value) > 16384 || strings.ContainsAny(value, "\x00\r\n") {
			return false
		}
		values = append(values, []byte(value))
		return true
	}
	fail := func() ([][]byte, error) {
		for _, v := range values {
			clear(v)
		}
		return nil, ErrAuthTransfer
	}
	if provider == Codex {
		for key := range root {
			switch key {
			case "auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh":
			default:
				return fail()
			}
		}
		var mode string
		if json.Unmarshal(root["auth_mode"], &mode) != nil || mode != "chatgpt" {
			return fail()
		}
		if key, ok := root["OPENAI_API_KEY"]; ok && !bytes.Equal(bytes.TrimSpace(key), []byte("null")) {
			return fail()
		}
		var tokens map[string]string
		if json.Unmarshal(root["tokens"], &tokens) != nil || len(tokens) != 4 {
			return fail()
		}
		for _, key := range []string{"id_token", "access_token", "refresh_token", "account_id"} {
			if !add(tokens[key]) {
				return fail()
			}
		}
		parts := strings.Split(tokens["id_token"], ".")
		if len(parts) != 3 {
			return fail()
		}
		claims, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil || len(claims) > 16384 {
			return fail()
		}
		defer clear(claims)
		var claimObject map[string]json.RawMessage
		if json.Unmarshal(claims, &claimObject) != nil || claimObject == nil {
			return fail()
		}
		var identity struct {
			Email string `json:"email"`
		}
		if uniqueJSONKeys(json.NewDecoder(bytes.NewReader(claims)), 0) != nil || json.Unmarshal(claims, &identity) != nil {
			return fail()
		}
		if identity.Email != "" && !add(identity.Email) {
			return fail()
		}
	} else {
		if len(root) != 1 {
			return fail()
		}
		var oauth struct {
			AccessToken      string   `json:"accessToken"`
			RefreshToken     string   `json:"refreshToken"`
			ExpiresAt        int64    `json:"expiresAt"`
			Scopes           []string `json:"scopes"`
			SubscriptionType string   `json:"subscriptionType"`
			RateLimitTier    string   `json:"rateLimitTier"`
		}
		decoder := json.NewDecoder(bytes.NewReader(root["claudeAiOauth"]))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&oauth) != nil || !add(oauth.AccessToken) || !add(oauth.RefreshToken) || oauth.ExpiresAt <= 0 || (oauth.SubscriptionType != "pro" && oauth.SubscriptionType != "max") {
			return fail()
		}
		inference := false
		for _, scope := range oauth.Scopes {
			if scope == "user:inference" {
				inference = true
			}
		}
		if !inference {
			return fail()
		}
	}
	return values, nil
}

func nativeCredentialCanarySeen(data []byte, values [][]byte) bool {
	for _, value := range values {
		if len(value) > 0 && bytes.Contains(data, value) {
			return true
		}
	}
	return false
}
