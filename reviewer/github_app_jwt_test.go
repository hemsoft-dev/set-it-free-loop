package reviewer

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"
)

var jwtSyntheticKey struct {
	once sync.Once
	key  *rsa.PrivateKey
	err  error
}

func appJWTTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	jwtSyntheticKey.once.Do(func() { jwtSyntheticKey.key, jwtSyntheticKey.err = rsa.GenerateKey(rand.Reader, 2048) })
	if jwtSyntheticKey.err != nil {
		t.Fatal(jwtSyntheticKey.err)
	}
	return jwtSyntheticKey.key
}

type appJWTTestSigner struct {
	public crypto.PublicKey
	key    *rsa.PrivateKey
	calls  int
	sign   func(io.Reader, []byte, crypto.SignerOpts) ([]byte, error)
}

func (s *appJWTTestSigner) Public() crypto.PublicKey { return s.public }
func (s *appJWTTestSigner) Sign(random io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	s.calls++
	if s.sign != nil {
		return s.sign(random, digest, opts)
	}
	if opts != crypto.SHA256 {
		return nil, errors.New("unexpected signature algorithm")
	}
	return s.key.Sign(random, digest, opts)
}
func (s *appJWTTestSigner) String() string { return "synthetic-private-capability-marker" }
func newAppJWTTestSigner(t *testing.T) *appJWTTestSigner {
	key := appJWTTestKey(t)
	return &appJWTTestSigner{public: &key.PublicKey, key: key}
}
func verifyAppJWT(t *testing.T, token []byte, key *rsa.PublicKey, at time.Time) {
	t.Helper()
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 || strings.Contains(string(token), "=") {
		t.Fatal("noncompact/padded JWT")
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if json.Unmarshal(head, &header) != nil || len(header) != 2 || header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatal("incorrect protected header")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil || len(claims) != 3 || claims["iss"] != "9002" || claims["iat"] != float64(at.Unix()-60) || claims["exp"] != float64(at.Unix()+540) {
		t.Fatal("incorrect claims")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], signature) != nil {
		t.Fatal("invalid independent RS256 signature")
	}
}

func TestGitHubAppJWTActualProducerClaimsSignatureAndTLSHandshake(t *testing.T) {
	key := newAppJWTTestSigner(t)
	signer, err := NewGitHubAppJWTSigner(alphaEnrollment(), key, func() time.Time { return controlTime })
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer clear(token)
	verifyAppJWT(t, token, &key.key.PublicKey, controlTime)
	if key.calls != 1 {
		t.Fatal("signing replayed")
	}
	for _, role := range []GitHubCredentialRole{GitHubReader, GitHubPublisher} {
		f, options := installationHTTPFixture(t, role, string(token))
		transport, err := AcquireGitHubInstallation(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		if transport.Receipt().Enrollment != alphaEnrollment() || transport.Receipt().Role != role {
			t.Fatal("signed App identity lost")
		}
		transport.Close()
		if f.calls != 7 || f.mints != 1 {
			t.Fatal("unexpected qualification calls", f.calls, f.mints)
		}
	}
}

func TestGitHubAppJWTExplicitInputsAndRSAKeyValidation(t *testing.T) {
	valid := newAppJWTTestSigner(t)
	badEnrollment := alphaEnrollment()
	badEnrollment.RepositoryID++
	for name, options := range map[string]struct {
		enrollment Enrollment
		key        crypto.Signer
		now        func() time.Time
	}{
		"wrong enrollment": {badEnrollment, valid, func() time.Time { return controlTime }},
		"nil key":          {alphaEnrollment(), nil, func() time.Time { return controlTime }},
		"typed nil key":    {alphaEnrollment(), (*rsa.PrivateKey)(nil), func() time.Time { return controlTime }},
		"nil clock":        {alphaEnrollment(), valid, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := NewGitHubAppJWTSigner(options.enrollment, options.key, options.now); got != nil || !errors.Is(err, ErrGitHubAppJWT) {
				t.Fatal("ambient/default input accepted")
			}
		})
	}
	oddModulus := func(bits int) *big.Int {
		return new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), uint(bits-1)), big.NewInt(1))
	}
	for name, public := range map[string]crypto.PublicKey{
		"non RSA": ed25519.PublicKey(make([]byte, 32)), "typed nil RSA": (*rsa.PublicKey)(nil), "missing modulus": &rsa.PublicKey{E: 65537},
		"small RSA": &rsa.PublicKey{N: oddModulus(1024), E: 65537}, "overlarge RSA": &rsa.PublicKey{N: oddModulus(8193), E: 65537},
		"even modulus": &rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), 2047), E: 65537}, "negative modulus": &rsa.PublicKey{N: new(big.Int).Neg(oddModulus(2048)), E: 65537},
		"even exponent": &rsa.PublicKey{N: oddModulus(2048), E: 4}, "missing exponent": &rsa.PublicKey{N: oddModulus(2048)},
	} {
		t.Run(name, func(t *testing.T) {
			key := &appJWTTestSigner{public: public}
			if signer, err := NewGitHubAppJWTSigner(alphaEnrollment(), key, func() time.Time { return controlTime }); signer != nil || !errors.Is(err, ErrGitHubAppJWT) || key.calls != 0 {
				t.Fatal("invalid RSA capability accepted")
			}
		})
	}
	if valid.calls != 0 {
		t.Fatal("constructor performed signing")
	}
}

func TestGitHubAppJWTRejectsSignerFailureWrongPaddingAndChangedPublicKey(t *testing.T) {
	for _, mode := range []string{"error", "wrong signature", "PSS", "changed public"} {
		t.Run(mode, func(t *testing.T) {
			key := newAppJWTTestSigner(t)
			signer, err := NewGitHubAppJWTSigner(alphaEnrollment(), key, func() time.Time { return controlTime })
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "error":
				key.sign = func(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
					return nil, errors.New("synthetic-private-capability-marker")
				}
			case "wrong signature":
				key.sign = func(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) { return make([]byte, 256), nil }
			case "PSS":
				key.sign = func(random io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
					return rsa.SignPSS(random, key.key, crypto.SHA256, digest, nil)
				}
			case "changed public":
				key.public = &rsa.PublicKey{N: new(big.Int).Add(key.key.N, big.NewInt(2)), E: 65537}
			}
			token, err := signer.Sign(context.Background())
			if len(token) != 0 || !errors.Is(err, ErrGitHubAppJWT) || strings.Contains(err.Error(), "synthetic-private-capability-marker") {
				t.Fatal("invalid signature or private error escaped")
			}
			expected := 1
			if mode == "changed public" {
				expected = 0
			}
			if key.calls != expected {
				t.Fatal("signing replayed", key.calls)
			}
		})
	}
}

func TestGitHubAppJWTCancellationAndClockBounds(t *testing.T) {
	for _, mode := range []string{"nil context", "cancelled", "deadline", "during signing", "during final clock", "zero clock", "backwards clock", "stale signature"} {
		t.Run(mode, func(t *testing.T) {
			key := newAppJWTTestSigner(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var contextToUse context.Context = ctx
			clockCalls := 0
			clock := func() time.Time {
				clockCalls++
				if mode == "zero clock" {
					return time.Time{}
				}
				if clockCalls == 2 {
					switch mode {
					case "backwards clock":
						return controlTime.Add(-time.Second)
					case "stale signature":
						return controlTime.Add(480 * time.Second)
					case "during final clock":
						cancel()
					}
				}
				return controlTime
			}
			signer, err := NewGitHubAppJWTSigner(alphaEnrollment(), key, clock)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "nil context":
				contextToUse = nil
			case "cancelled":
				cancel()
			case "deadline":
				var expired context.CancelFunc
				contextToUse, expired = context.WithDeadline(context.Background(), time.Unix(1, 0))
				defer expired()
			case "during signing":
				key.sign = func(random io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
					cancel()
					return key.key.Sign(random, digest, opts)
				}
			}
			token, err := signer.Sign(contextToUse)
			if len(token) != 0 || !errors.Is(err, ErrGitHubAppJWT) {
				t.Fatal("unqualified/cancelled result returned")
			}
			if (mode == "cancelled" || mode == "during signing" || mode == "during final clock") && !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation cause lost")
			}
			if mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("deadline cause lost")
			}
			if (mode == "cancelled" || mode == "deadline" || mode == "nil context" || mode == "zero clock") && key.calls != 0 {
				t.Fatal("refused request still signed")
			}
			if key.calls > 1 {
				t.Fatal("signing replayed")
			}
		})
	}
	var nilSigner *GitHubAppJWTSigner
	for _, signer := range []*GitHubAppJWTSigner{nilSigner, {}} {
		if token, err := signer.Sign(context.Background()); len(token) != 0 || !errors.Is(err, ErrGitHubAppJWT) {
			t.Fatal("empty signer accepted")
		}
	}
}

func TestGitHubAppJWTHandleFormattingRedactsCapability(t *testing.T) {
	key := newAppJWTTestSigner(t)
	signer, err := NewGitHubAppJWTSigner(alphaEnrollment(), key, func() time.Time { return controlTime })
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{signer, *signer} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{fmt.Sprint(value), fmt.Sprintf("%+v", value), fmt.Sprintf("%#v", value), string(raw)} {
			if strings.Contains(text, "synthetic-private-capability-marker") || !strings.Contains(text, "redacted") {
				t.Fatal("signing capability exposed")
			}
		}
	}
}
