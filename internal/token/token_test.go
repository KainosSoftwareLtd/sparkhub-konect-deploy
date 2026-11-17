package token

import (
	"fmt"
	"strings"
	"testing"
)

const good = "kit_acme-uk.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234"

func TestParse(t *testing.T) {
	tok, err := Parse(good)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Org != "acme-uk" || tok.TokenID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("parsed %+v", tok)
	}
	if tok.Bearer() != "Bearer "+good {
		t.Fatal("bearer")
	}
	for _, s := range []string{fmt.Sprint(tok), fmt.Sprintf("%v %s", tok, tok), tok.Redacted()} {
		if strings.Contains(s, "abcdefghijklmnop") {
			t.Fatalf("secret leaked: %s", s)
		}
	}
	if tok.Redacted() != "kit_acme-uk.01234567…" {
		t.Fatalf("redacted %q", tok.Redacted())
	}
}

func TestParseRefuses(t *testing.T) {
	bad := []string{
		"",
		"kur_acme.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234",
		"kit_Acme.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234",  // upper-case org
		"kit_acme.0123456789ABCDEF0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234",  // upper-case id
		"kit_acme.0123456789abcdef0123456789abcde.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234",   // id too short
		"kit_acme.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_0123",   // secret too short
		"kit_acme.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_0123=",  // padding
		"kit_ac.me.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234", // dot in org
		"kit_-acme.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234",
		" " + good,
		good + "\n",
		"kit_evil.com/x.0123456789abcdef0123456789abcdef.abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_01234",
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("accepted %q", s)
		} else if strings.Contains(err.Error(), "abcdefghij") {
			t.Errorf("error echoes the input")
		}
	}
}

func TestPattern(t *testing.T) {
	if !Pattern.MatchString("x=" + good + ";") {
		t.Fatal("pattern misses an embedded token")
	}
}
