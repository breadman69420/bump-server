// verifykey reads ED25519_PRIVATE_KEY from the environment, loads it with
// the same crypto.NewSigner the server uses at startup, and prints the
// matching public key in the Kotlin byteArrayOf format used by the Android
// client at TokenVerifier.SERVER_PUBLIC_KEY. This is the offline equivalent
// of OPERATIONS.md §6 — use it before a first deploy to confirm the client
// constant matches the private key you are about to set as a Fly secret.
//
// Usage:
//
//	ED25519_PRIVATE_KEY='<base64>' go run ./cmd/verifykey
//
// The tool prints ONLY the public key. It does not print the private key
// and does not write any file. Keep the private key in your password
// manager; do not paste it into a shell where it could land in history.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/ttalvac/bump-server/crypto"
)

func main() {
	priv := os.Getenv("ED25519_PRIVATE_KEY")
	if priv == "" {
		log.Fatal("ED25519_PRIVATE_KEY env var is required")
	}
	signer, err := crypto.NewSigner(priv)
	if err != nil {
		log.Fatalf("load signer: %v", err)
	}
	fmt.Println(crypto.FormatKotlinByteArray(signer.PublicKeyBytes()))
}
