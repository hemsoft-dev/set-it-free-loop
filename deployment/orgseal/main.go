package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"time"

	"golang.org/x/crypto/nacl/box"
)

//go:embed recipient-policy.json
var recipientPolicy []byte

type policy struct {
	Organization          string  `json:"organization"`
	OrganizationID        int64   `json:"organization_id"`
	KeyID                 string  `json:"key_id"`
	PublicKey             string  `json:"public_key"`
	PublicKeySHA256       string  `json:"public_key_sha256"`
	SelectedRepositoryIDs []int64 `json:"selected_repository_ids"`
	SourceRepositoryID    int64   `json:"source_repository_id"`
	AppID                 int64   `json:"app_id"`
	ClientID              string  `json:"client_id"`
}

type context struct {
	Repository  string `json:"source_repository"`
	Revision    string `json:"reviewed_sha"`
	RunID       int64  `json:"run_id"`
	RunAttempt  int64  `json:"run_attempt"`
	WorkflowRef string `json:"workflow_ref"`
}

type envelope struct {
	Policy         policy  `json:"recipient"`
	Context        context `json:"context"`
	Schema         string  `json:"schema"`
	Encryption     string  `json:"encryption"`
	EncryptedValue string  `json:"encrypted_value"`
	ObservedAt     string  `json:"observed_at"`
}

func validatePolicy(raw []byte) (policy, [32]byte, error) {
	var p policy
	var key [32]byte
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, key, errors.New("invalid recipient policy")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return p, key, errors.New("extra recipient policy data")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(p.PublicKey)
	digest := sha256.Sum256(decoded)
	if err != nil || len(decoded) != len(key) ||
		p.Organization != "hemsoft-dev" || p.OrganizationID != 338855369 ||
		p.SourceRepositoryID != 1169772257 || p.AppID != 4448946 || p.ClientID != "Iv23liwvwJJUh2bUIKLW" ||
		p.KeyID != "3380204578043523366" ||
		p.PublicKeySHA256 != "a476bafb28bb53ef81e74a18c7cf84440f8322dedcd82a381196c37d4072a0d1" ||
		hex.EncodeToString(digest[:]) != p.PublicKeySHA256 || !slices.Equal(p.SelectedRepositoryIDs, []int64{1408025382}) {
		return p, key, errors.New("unapproved recipient policy")
	}
	copy(key[:], decoded)
	return p, key, nil
}

func validateContext(getenv func(string) string) (context, error) {
	var c context
	c.Repository = getenv("GITHUB_REPOSITORY")
	c.Revision = getenv("GITHUB_SHA")
	c.WorkflowRef = getenv("GITHUB_WORKFLOW_REF")
	var err error
	c.RunID, err = strconv.ParseInt(getenv("GITHUB_RUN_ID"), 10, 64)
	if err != nil {
		return c, errors.New("invalid trusted run context")
	}
	c.RunAttempt, err = strconv.ParseInt(getenv("GITHUB_RUN_ATTEMPT"), 10, 64)
	if err != nil || c.RunID < 1 || c.RunAttempt < 1 ||
		getenv("GITHUB_REPOSITORY_ID") != "1169772257" || getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" ||
		getenv("GITHUB_REF") != "refs/heads/main" || getenv("GITHUB_ACTOR") != "HemSoft" || getenv("GITHUB_TRIGGERING_ACTOR") != "HemSoft" ||
		(c.Repository != "HemSoft/set-it-free-loop" && c.Repository != "hemsoft-dev/set-it-free-loop") ||
		!regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(c.Revision) ||
		c.WorkflowRef != c.Repository+"/.github/workflows/seal-sfl-org-credential.yml@refs/heads/main" {
		return c, errors.New("invalid trusted run context")
	}
	return c, nil
}

func validatePrivateKey(message []byte) error {
	if len(message) == 0 || len(message) > 65536 {
		return errors.New("invalid credential input")
	}
	b, rest := pem.Decode(message)
	if b == nil || len(bytes.TrimSpace(rest)) != 0 || len(b.Headers) != 0 {
		return errors.New("invalid credential input")
	}
	var key *rsa.PrivateKey
	if b.Type == "RSA PRIVATE KEY" {
		key, _ = x509.ParsePKCS1PrivateKey(b.Bytes)
	} else if b.Type == "PRIVATE KEY" {
		parsed, _ := x509.ParsePKCS8PrivateKey(b.Bytes)
		key, _ = parsed.(*rsa.PrivateKey)
	}
	if key == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return errors.New("invalid credential input")
	}
	return nil
}

func seal(message []byte, key *[32]byte, random io.Reader) (string, error) {
	if err := validatePrivateKey(message); err != nil {
		return "", err
	}
	ciphertext, err := box.SealAnonymous(nil, message, key, random)
	if err != nil {
		return "", errors.New("credential sealing failed")
	}
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func prepare(rawPolicy []byte, getenv func(string) string) (envelope, error) {
	var result envelope
	p, key, err := validatePolicy(rawPolicy)
	if err != nil {
		return result, err
	}
	c, err := validateContext(getenv)
	if err != nil {
		return result, err
	}
	message := []byte(getenv("SFL_APP_PRIVATE_KEY"))
	defer clear(message)
	encrypted, err := seal(message, &key, rand.Reader)
	if err != nil {
		return result, err
	}
	return envelope{Policy: p, Context: c, Schema: "sealed_sfl_app_credential_v1",
		Encryption: "libsodium_crypto_box_seal", EncryptedValue: encrypted, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
}

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "Credential sealing failed: unexpected arguments.")
		os.Exit(1)
	}
	result, err := prepare(recipientPolicy, os.Getenv)
	os.Unsetenv("SFL_APP_PRIVATE_KEY")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Credential sealing failed; no credential is reported.")
		os.Exit(1)
	}
	f, err := os.OpenFile("sealed-sfl-app-credential.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Credential ciphertext output could not be created.")
		os.Exit(1)
	}
	err = json.NewEncoder(f).Encode(result)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		os.Remove("sealed-sfl-app-credential.json")
		fmt.Fprintln(os.Stderr, "Credential ciphertext output failed.")
		os.Exit(1)
	}
	fmt.Println("Sealed credential for the reviewed selected recipient. No organization secret was written.")
}
