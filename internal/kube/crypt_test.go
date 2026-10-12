package kube

import "testing"

func TestSHA512Crypt(t *testing.T) {
	want := "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"
	if got := sha512Crypt("Hello world!", "saltstring"); got != want {
		t.Fatalf("got %s", got)
	}
}
