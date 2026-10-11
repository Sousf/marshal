// Package tapkey makes the SSH deploy key a release workflow uses to push
// its formula to the Homebrew tap. The key is generated in memory and
// handed straight to gh, so the private half never touches disk.
package tapkey

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
)

// Generate returns an ed25519 key pair: the public half as an
// authorized_keys line, the private half in OpenSSH's own format, which is
// what ssh and GoReleaser read.
func Generate(comment string) (public, private []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	blob := pubBlob(pub)
	public = []byte("ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + comment + "\n")

	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return nil, nil, err
	}
	var sec bytes.Buffer
	sec.Write(check[:])
	sec.Write(check[:])
	putString(&sec, []byte("ssh-ed25519"))
	putString(&sec, pub)
	putString(&sec, priv) // seed then public key, as OpenSSH stores it
	putString(&sec, []byte(comment))
	for i := byte(1); sec.Len()%8 != 0; i++ {
		sec.WriteByte(i)
	}

	var b bytes.Buffer
	b.WriteString("openssh-key-v1\x00")
	putString(&b, []byte("none")) // cipher
	putString(&b, []byte("none")) // kdf
	putString(&b, nil)            // kdf options
	binary.Write(&b, binary.BigEndian, uint32(1))
	putString(&b, blob)
	putString(&b, sec.Bytes())
	private = pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: b.Bytes()})
	return public, private, nil
}

func pubBlob(pub ed25519.PublicKey) []byte {
	var b bytes.Buffer
	putString(&b, []byte("ssh-ed25519"))
	putString(&b, pub)
	return b.Bytes()
}

func putString(b *bytes.Buffer, s []byte) {
	binary.Write(b, binary.BigEndian, uint32(len(s)))
	b.Write(s)
}
