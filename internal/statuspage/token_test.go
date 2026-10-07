package statuspage

import "testing"

func TestTokenIsPerHostAndPerSecret(t *testing.T) {
	a := Token("secret", "vps")
	if a != Token("secret", "vps") {
		t.Fatal("the same secret and host must derive the same token")
	}
	if a == Token("secret", "ks") {
		t.Error("two hosts share a token")
	}
	if a == Token("other", "vps") {
		t.Error("rotating the secret did not change the token")
	}
	if h := TokenHash(a); h == a || len(h) != 64 {
		t.Errorf("TokenHash(%q) = %q, want a distinct sha256 hex digest", a, h)
	}
}
