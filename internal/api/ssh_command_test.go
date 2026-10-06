package api

import "testing"

// NTG-194: the command handed out for copying must reach IOL 15 from a stock OpenSSH 9 client.
func TestSSHAccessCommandCarriesLegacyAlgorithms(t *testing.T) {
	got := sshAccessCommand(2223, "admin", "10.70.38.8")
	want := "ssh -o KexAlgorithms=+diffie-hellman-group14-sha1,diffie-hellman-group-exchange-sha1 -o HostKeyAlgorithms=+ssh-rsa -p 2223 admin@10.70.38.8"
	if got != want {
		t.Fatalf("sshAccessCommand = %q, want %q", got, want)
	}
}
