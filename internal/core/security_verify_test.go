package core

import "testing"

// A password that really starts/ends with a space must work.
func TestVerifyAdmin_WhitespacePasswords(t *testing.T) {
	spaced, err := GetPasswordHash(" pass word ")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyAdmin(" pass word ", spaced); err != nil {
		t.Fatalf("exact password with spaces refused: %v", err)
	}
	if VerifyAdmin("pass word", spaced) == nil {
		t.Fatal("trimmed variant must not match a password set with spaces")
	}

	plain, _ := GetPasswordHash("password1")
	if err := VerifyAdmin("password1 \n", plain); err != nil {
		t.Fatalf("stray trailing whitespace should still be tolerated: %v", err)
	}
	if VerifyAdmin("password2", plain) == nil {
		t.Fatal("wrong password accepted")
	}
}
