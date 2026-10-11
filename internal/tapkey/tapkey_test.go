package tapkey

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ssh-keygen is the reference reader: if it derives the same public key
// from the private one, ssh and GoReleaser will accept it too.
func TestKeyReadsBackWithSSHKeygen(t *testing.T) {
	pub, priv, err := Generate("goreleaser x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(pub), "ssh-ed25519 ") || !strings.HasPrefix(string(priv), "-----BEGIN OPENSSH PRIVATE KEY-----") {
		t.Fatalf("pub=%q", pub)
	}
	bin, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	path := filepath.Join(t.TempDir(), "k")
	os.WriteFile(path, priv, 0o600)
	out, err := exec.Command(bin, "-y", "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	want := strings.Join(strings.Fields(string(pub))[:2], " ")
	if got := strings.Join(strings.Fields(string(out))[:2], " "); got != want {
		t.Errorf("derived %q, want %q", got, want)
	}
}
