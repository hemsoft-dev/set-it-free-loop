package reviewer

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"reflect"
	"strconv"
	"time"
)

var ErrGitHubAppJWT = errors.New("explicit GitHub App signing qualification required")

// GitHubAppJWTSigner has no key discovery, persistence or network behavior.
// The trusted caller owns a bounded signing capability and clock. Construction
// and signing do not qualify the key/App association or authorize activation.
type GitHubAppJWTSigner struct {
	appID  int64
	key    crypto.Signer
	public rsa.PublicKey
	now    func() time.Time
}

func (GitHubAppJWTSigner) String() string   { return "GitHubAppJWTSigner{redacted}" }
func (GitHubAppJWTSigner) GoString() string { return "GitHubAppJWTSigner{redacted}" }
func (GitHubAppJWTSigner) MarshalJSON() ([]byte, error) {
	return []byte(`"GitHubAppJWTSigner{redacted}"`), nil
}

func validGitHubRSAKey(key crypto.PublicKey) (*rsa.PublicKey, bool) {
	public, ok := key.(*rsa.PublicKey)
	if !ok || public == nil || public.N == nil || public.N.Sign() <= 0 || public.N.Bit(0) != 1 || public.N.BitLen() < 2048 || public.N.BitLen() > 8192 || public.E < 3 || public.E > 1<<31-1 || public.E%2 == 0 {
		return nil, false
	}
	return public, true
}

func nilGitHubSigner(key crypto.Signer) bool {
	if key == nil {
		return true
	}
	value := reflect.ValueOf(key)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	}
	return false
}

// NewGitHubAppJWTSigner accepts only an explicit enrolled identity, RSA signing
// capability and clock. The local public-key bound is 2048..8192 bits. The signer
// stays caller-owned; callers must serialize access if their capability needs it.
func NewGitHubAppJWTSigner(enrollment Enrollment, key crypto.Signer, now func() time.Time) (*GitHubAppJWTSigner, error) {
	if enrollment.Validate() != nil || nilGitHubSigner(key) || now == nil {
		return nil, ErrGitHubAppJWT
	}
	public, ok := validGitHubRSAKey(key.Public())
	if !ok {
		return nil, ErrGitHubAppJWT
	}
	return &GitHubAppJWTSigner{enrollment.AppID, key, rsa.PublicKey{N: new(big.Int).Set(public.N), E: public.E}, now}, nil
}

func githubJWTContextError(ctx context.Context) error {
	if ctx == nil {
		return ErrGitHubAppJWT
	}
	if err := ctx.Err(); err != nil {
		// Do not include arbitrary caller error text in credential diagnostics.
		if errors.Is(err, context.Canceled) {
			return errors.Join(ErrGitHubAppJWT, context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.Join(ErrGitHubAppJWT, context.DeadlineExceeded)
		}
		return ErrGitHubAppJWT
	}
	return nil
}

// Sign produces an RS256 JWT for AcquireGitHubInstallation. It backdates iat by
// sixty seconds and expires nine minutes after the captured clock. It verifies
// the returned signature against the captured public key. Context cancellation
// is checked around signing; crypto.Signer itself has no cancellation API, so
// the supplied capability must bound its own operation. No retry is performed.
// The caller owns the returned JWT bytes. Go may retain immutable temporary
// strings; neither this API nor clearing the result promises secure erasure.
func (s *GitHubAppJWTSigner) Sign(ctx context.Context) ([]byte, error) {
	if err := githubJWTContextError(ctx); err != nil {
		return nil, err
	}
	if s == nil || nilGitHubSigner(s.key) || s.now == nil || s.appID <= 0 {
		return nil, ErrGitHubAppJWT
	}
	public, ok := validGitHubRSAKey(s.key.Public())
	if !ok || public.E != s.public.E || public.N.Cmp(s.public.N) != 0 {
		return nil, ErrGitHubAppJWT
	}
	at := s.now()
	if at.Unix() < 60 || at.Year() > 9999 {
		return nil, ErrGitHubAppJWT
	}
	header := []byte(`{"alg":"RS256","typ":"JWT"}`)
	claims, _ := json.Marshal(struct {
		Issuer  string `json:"iss"`
		Issued  int64  `json:"iat"`
		Expires int64  `json:"exp"`
	}{strconv.FormatInt(s.appID, 10), at.Unix() - 60, at.Unix() + 540})
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	if err := githubJWTContextError(ctx); err != nil {
		return nil, err
	}
	signature, err := s.key.Sign(rand.Reader, digest[:], crypto.SHA256)
	if cancelErr := githubJWTContextError(ctx); cancelErr != nil {
		return nil, cancelErr
	}
	if err != nil || rsa.VerifyPKCS1v15(&s.public, crypto.SHA256, digest[:], signature) != nil {
		return nil, ErrGitHubAppJWT
	}
	// Refuse a stale result or a backwards clock, even after a slow signer.
	checked := s.now()
	if checked.Unix() < at.Unix() || checked.Unix() >= at.Unix()+480 {
		return nil, ErrGitHubAppJWT
	}
	if err := githubJWTContextError(ctx); err != nil {
		return nil, err
	}
	return []byte(signing + "." + base64.RawURLEncoding.EncodeToString(signature)), nil
}
