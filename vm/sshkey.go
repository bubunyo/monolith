package vm

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
)

// ensureSSHKey returns the base64 body of the VM's ed25519 public key (the
// part after "ssh-ed25519 "), generating the key pair on first use.
func ensureSSHKey(name string) (string, error) {
	keyPath, err := VMSSHKeyPath(name)
	if err != nil {
		return "", err
	}
	pubPath := keyPath + ".pub"
	if data, err := os.ReadFile(pubPath); err == nil {
		return pubKeyBody(string(data))
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "monolith "+name)
	if err != nil {
		return "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0600); err != nil {
		return "", err
	}
	authorized := ssh.MarshalAuthorizedKey(sshPub)
	if err := os.WriteFile(pubPath, authorized, 0644); err != nil {
		return "", err
	}
	return pubKeyBody(string(authorized))
}

func pubKeyBody(line string) (string, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != ssh.KeyAlgoED25519 {
		return "", fmt.Errorf("unexpected public key %q", strings.TrimSpace(line))
	}
	return fields[1], nil
}
