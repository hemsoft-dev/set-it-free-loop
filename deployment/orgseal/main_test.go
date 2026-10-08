package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/crypto/nacl/box"
)

func testCredential(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func testEnvironment() map[string]string {
	return map[string]string{"GITHUB_REPOSITORY": "HemSoft/set-it-free-loop", "GITHUB_SHA": strings.Repeat("a", 40),
		"GITHUB_RUN_ID": "1", "GITHUB_RUN_ATTEMPT": "1", "GITHUB_REPOSITORY_ID": "1169772257",
		"GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_REF": "refs/heads/main", "GITHUB_ACTOR": "HemSoft", "GITHUB_TRIGGERING_ACTOR": "HemSoft",
		"GITHUB_WORKFLOW_REF": "HemSoft/set-it-free-loop/.github/workflows/seal-sfl-org-credential.yml@refs/heads/main"}
}

func TestUnapprovedRecipientNeverReadsCredential(t *testing.T) {
	for _, field := range []string{"organization_id", "key_id", "public_key", "public_key_sha256", "selected_repository_ids", "app_id", "source_repository_id", "client_id"} {
		t.Run(field, func(t *testing.T) {
			var p map[string]any
			if err := json.Unmarshal(recipientPolicy, &p); err != nil {
				t.Fatal(err)
			}
			if field == "selected_repository_ids" {
				p[field] = []int64{1408029795}
			} else {
				p[field] = "unexpected"
			}
			raw, _ := json.Marshal(p)
			reads := 0
			_, err := prepare(raw, func(name string) string {
				if name == "SFL_APP_PRIVATE_KEY" {
					reads++
				}
				return testEnvironment()[name]
			})
			if err == nil || reads != 0 {
				t.Fatal("Unapproved recipient read the credential or passed")
			}
		})
	}
}

func TestUntrustedContextNeverReadsCredential(t *testing.T) {
	for _, field := range []string{"GITHUB_REPOSITORY", "GITHUB_SHA", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_REPOSITORY_ID",
		"GITHUB_EVENT_NAME", "GITHUB_REF", "GITHUB_ACTOR", "GITHUB_TRIGGERING_ACTOR", "GITHUB_WORKFLOW_REF"} {
		t.Run(field, func(t *testing.T) {
			env := testEnvironment()
			env[field] = "untrusted"
			reads := 0
			_, err := prepare(recipientPolicy, func(name string) string {
				if name == "SFL_APP_PRIVATE_KEY" {
					reads++
				}
				return env[name]
			})
			if err == nil || reads != 0 {
				t.Fatal("Untrusted context read the credential or passed")
			}
		})
	}
}

func TestSealingInteroperabilityAndTampering(t *testing.T) {
	public, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	message := testCredential(t)
	first, err := seal(message, public, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	second, err := seal(message, public, rand.Reader)
	if err != nil || first == second {
		t.Fatal("Anonymous sealing was not randomized")
	}
	ciphertext, _ := base64.StdEncoding.DecodeString(first)
	opened, ok := box.OpenAnonymous(nil, ciphertext, public, private)
	if !ok || string(opened) != string(message) {
		t.Fatal("Synthetic credential did not round trip")
	}
	otherPublic, otherPrivate, _ := box.GenerateKey(rand.Reader)
	if _, ok := box.OpenAnonymous(nil, ciphertext, otherPublic, otherPrivate); ok {
		t.Fatal("Wrong recipient accepted ciphertext")
	}
	changed := append([]byte(nil), ciphertext...)
	changed[len(changed)-1] ^= 1
	if _, ok := box.OpenAnonymous(nil, changed, public, private); ok {
		t.Fatal("Tampered ciphertext was accepted")
	}
	input, _ := json.Marshal(map[string][]byte{"public": public[:], "private": private[:], "message": message, "ciphertext": ciphertext})
	command := exec.Command("python3", "test-interop.py")
	command.Stdin = strings.NewReader(string(input))
	if out, err := command.CombinedOutput(); err != nil || string(out) != "ok\n" {
		t.Fatal("Independent libsodium compatibility failed")
	}
}

func TestInvalidCredentialProducesNoCiphertext(t *testing.T) {
	key, _, _ := box.GenerateKey(rand.Reader)
	for _, message := range [][]byte{nil, []byte("not a private key"), []byte(strings.Repeat("x", 65537))} {
		result, err := seal(message, key, rand.Reader)
		if err == nil || result != "" {
			t.Fatal("Invalid credential produced ciphertext")
		}
	}
}

func TestPreparedEnvelopeContainsCiphertextAndReviewedContext(t *testing.T) {
	env := testEnvironment()
	credential := testCredential(t)
	env["SFL_APP_PRIVATE_KEY"] = string(credential)
	result, err := prepare(recipientPolicy, func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "PRIVATE KEY") || strings.Contains(string(raw), string(credential)) ||
		result.Context.Revision != env["GITHUB_SHA"] || result.Encryption != "libsodium_crypto_box_seal" || result.EncryptedValue == "" {
		t.Fatal("Envelope leaked plaintext or lost its reviewed context")
	}
}
