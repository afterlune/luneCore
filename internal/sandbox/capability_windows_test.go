//go:build windows

package sandbox

import "testing"

func TestDeriveInternetClientCapabilitySID(t *testing.T) {
	sid, err := deriveCapabilitySID("internetClient")
	if err != nil {
		t.Fatal(err)
	}
	if got := sid.String(); got != "S-1-15-3-1" {
		t.Fatalf("internetClient SID = %s", got)
	}
}
