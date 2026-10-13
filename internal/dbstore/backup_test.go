package dbstore

import "testing"

func TestPackRoundTrip(t *testing.T) {
	recs := map[string]Record{"argus-orgs": {Data: map[string][]byte{"orgs": []byte(`[{"id":"default"}]`)}}, "argus-sessions": {}}
	b, err := Pack(recs, "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(b, "wrong-passphrase-123"); err == nil {
		t.Error("wrong passphrase accepted")
	}
	got, err := Unpack(b, "correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got["argus-orgs"].Data["orgs"]) != `[{"id":"default"}]` {
		t.Errorf("round trip lost data or kept sessions: %+v", got)
	}
}
