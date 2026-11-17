// Package token parses install tokens (design #1702 §4.1): kit_<orgCode>.<tokenId>.<secret>.
//
// tokenId is 16 random bytes in lower-case hex (32 characters); secret is 32 random bytes in base64url without
// padding (43 characters). The org code names the SparkHub host to call. The token itself is never logged:
// Redacted() is the only form that may appear in a message.
package token

import (
	"errors"
	"regexp"
)

var re = regexp.MustCompile(`^kit_([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)\.([0-9a-f]{32})\.([A-Za-z0-9_-]{43})$`)

// Pattern finds a token anywhere in a string (used to drop stray copies from the environment).
var Pattern = regexp.MustCompile(`kit_[a-z0-9-]{1,63}\.[0-9a-f]{32}\.[A-Za-z0-9_-]{43}`)

// Token is a parsed install token.
type Token struct {
	Org     string
	TokenID string
	raw     string
}

// ErrInvalid is returned for anything that is not a well-formed install token.
var ErrInvalid = errors.New("KONECT_INSTALL_TOKEN is not an install token (expected kit_<org>.<id>.<secret>, created in SparkHub → Konect → Install)")

// Parse checks the format. It never echoes the input.
func Parse(s string) (Token, error) {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return Token{}, ErrInvalid
	}
	return Token{Org: m[1], TokenID: m[2], raw: s}, nil
}

// Bearer is the Authorization header value.
func (t Token) Bearer() string { return "Bearer " + t.raw }

// Redacted is the form shown in logs and messages: kit_<org>.<first 8 of tokenId>…, as the Install page shows it.
func (t Token) Redacted() string { return "kit_" + t.Org + "." + t.TokenID[:8] + "…" }

// String never returns the secret.
func (t Token) String() string { return t.Redacted() }
