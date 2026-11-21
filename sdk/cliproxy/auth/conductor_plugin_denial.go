package auth

import "strings"

// isProviderCodedCredentialDenial reports whether a credential-scoped non-429
// failure carries a provider-assigned code, such as a plugin's 403
// upgrade_required with scope "credential". The fork cools such denials only
// for the denied credential/model pair, so the credential stays eligible for
// sibling models. Host-classified failures (uncoded, credential_quota,
// force_cooldown, validation_required) keep the credential-wide retry state.
func isProviderCodedCredentialDenial(err *Error) bool {
	if err == nil || isForceCooldownError(err) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(err.Code)) {
	case "", credentialQuotaErrorCode, "validation_required":
		return false
	}
	return true
}
