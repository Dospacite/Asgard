package auth

import "testing"

func TestPasswordHashRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("valid password rejected")
	}
	if VerifyPassword(hash, "wrong password entirely") {
		t.Fatal("invalid password accepted")
	}
}
func TestPasswordMinimum(t *testing.T) {
	if _, err := HashPassword("too-short"); err == nil {
		t.Fatal("short password accepted")
	}
}
