package sealed

import "testing"

func TestSealRoundTrip(t *testing.T) {
	sealed, err := Seal("correct horse battery", []byte("fleet credentials"))
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := Open("correct horse battery", sealed); err != nil || string(plain) != "fleet credentials" {
		t.Fatalf("round trip failed: %q %v", plain, err)
	}
	if _, err := Open("wrong passphrase", sealed); err == nil {
		t.Fatal("opened with the wrong passphrase")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := Open("correct horse battery", sealed); err == nil {
		t.Fatal("opened a tampered backup")
	}
}
