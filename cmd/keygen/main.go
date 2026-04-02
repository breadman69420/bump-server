package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ttalvac/bump-server/crypto"
)

func main() {
	pub, priv, err := crypto.GenerateKeypair()
	if err != nil {
		log.Fatalf("Failed to generate keypair: %v", err)
	}

	privB64 := base64.StdEncoding.EncodeToString(priv)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	fmt.Println("=== Ed25519 Keypair for Bump ===")
	fmt.Println()
	fmt.Println("--- Private Key (base64, for ED25519_PRIVATE_KEY env var) ---")
	fmt.Println(privB64)
	fmt.Println()
	fmt.Println("--- Public Key (base64) ---")
	fmt.Println(pubB64)
	fmt.Println()
	fmt.Println("--- Public Key (Kotlin byte array literal for TokenVerifier.kt) ---")
	fmt.Print("private val SERVER_PUBLIC_KEY = byteArrayOf(\n    ")
	parts := make([]string, len(pub))
	for i, b := range pub {
		if b > 127 {
			parts[i] = fmt.Sprintf("%d.toByte()", int(b)-256)
		} else {
			parts[i] = fmt.Sprintf("%d", b)
		}
	}
	// Print 8 bytes per line
	for i := 0; i < len(parts); i += 8 {
		end := i + 8
		if end > len(parts) {
			end = len(parts)
		}
		line := strings.Join(parts[i:end], ", ")
		if end < len(parts) {
			fmt.Printf("%s,\n    ", line)
		} else {
			fmt.Printf("%s\n", line)
		}
	}
	fmt.Println(")")
	fmt.Println()

	// Optionally save to files
	if len(os.Args) > 1 && os.Args[1] == "--save" {
		os.WriteFile("private.key", []byte(privB64), 0600)
		os.WriteFile("public.key", []byte(pubB64), 0644)
		fmt.Println("Saved private.key and public.key")
	}
}
