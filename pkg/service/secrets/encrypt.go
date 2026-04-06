package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	ossModels "go.keploy.io/server/v3/pkg/models"
)

const (
	// EncryptedPrefix is the sentinel marker for encrypted values.
	// During replay, any string starting with this prefix is decrypted.
	// Uses a UUID-like format to be extremely unlikely in real plaintext data.
	EncryptedPrefix = "$KEPLOY_ENC_v1_7f3a9b2e$"

	algorithmAES256GCM = "aes-256-gcm"
)

// EncryptionMetadata is stored alongside the test set so the replay side
// can recover the data encryption key from OpenBao.
type EncryptionMetadata struct {
	WrappedDEK string `json:"wrapped_dek" yaml:"wrapped_dek" bson:"wrapped_dek"`
	KeyVersion int    `json:"key_version" yaml:"key_version" bson:"key_version"`
	Algorithm  string `json:"algorithm" yaml:"algorithm" bson:"algorithm"`
	AppID      string `json:"app_id" yaml:"app_id" bson:"app_id"`
}

// EncryptionEngine performs AES-256-GCM encryption using a data encryption key (DEK)
// obtained from OpenBao via envelope encryption.
type EncryptionEngine struct {
	gcm        cipher.AEAD
	wrappedDEK string
	keyVersion int
	appID      string
}

// NewEncryptionEngine creates an engine from a plaintext data encryption key.
// The wrappedDEK and keyVersion come from OpenBao's GenerateDataKey response.
func NewEncryptionEngine(plaintextDEK []byte, wrappedDEK string, keyVersion int, appID string) (*EncryptionEngine, error) {
	if len(plaintextDEK) != 32 {
		return nil, fmt.Errorf("AES-256 requires a 32-byte key, got %d bytes", len(plaintextDEK))
	}
	block, err := aes.NewCipher(plaintextDEK)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}
	return &EncryptionEngine{
		gcm:        gcm,
		wrappedDEK: wrappedDEK,
		keyVersion: keyVersion,
		appID:      appID,
	}, nil
}

// Process encrypts a plaintext value and returns the sentinel-prefixed ciphertext.
func (e *EncryptionEngine) Process(plaintext string) (string, error) {
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to generate nonce: %w", err)
	}
	ciphertext := e.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	encoded := base64.StdEncoding.EncodeToString(ciphertext)
	return EncryptedPrefix + encoded, nil
}

// Decrypt restores a sentinel-prefixed encrypted value to its original plaintext.
func (e *EncryptionEngine) Decrypt(encrypted string) (string, error) {
	if !strings.HasPrefix(encrypted, EncryptedPrefix) {
		return encrypted, nil // not encrypted
	}
	encoded := strings.TrimPrefix(encrypted, EncryptedPrefix)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("failed to decode ciphertext: %w", err)
	}
	nonceSize := e.gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := e.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}
	return string(plaintext), nil
}

// Metadata returns the encryption metadata for this session.
func (e *EncryptionEngine) Metadata() *EncryptionMetadata {
	return &EncryptionMetadata{
		WrappedDEK: e.wrappedDEK,
		KeyVersion: e.keyVersion,
		Algorithm:  algorithmAES256GCM,
		AppID:      e.appID,
	}
}

// IsEncrypted checks whether a string value has the encryption sentinel prefix.
func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, EncryptedPrefix)
}

// Decryptor wraps an EncryptionEngine for replay-side decryption only.
type Decryptor struct {
	engine *EncryptionEngine
}

// NewDecryptor creates a Decryptor from a plaintext DEK (recovered from OpenBao).
func NewDecryptor(plaintextDEK []byte, meta *EncryptionMetadata) (*Decryptor, error) {
	eng, err := NewEncryptionEngine(plaintextDEK, meta.WrappedDEK, meta.KeyVersion, meta.AppID)
	if err != nil {
		return nil, err
	}
	return &Decryptor{engine: eng}, nil
}

// DecryptValue decrypts a single value if it has the encryption prefix.
func (d *Decryptor) DecryptValue(value string) (string, error) {
	return d.engine.Decrypt(value)
}

func isBase64Char(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
		(c >= '0' && c <= '9') || c == '+' || c == '/' || c == '='
}

// DecryptTestCase decrypts all encrypted values in a test case in-place.
// Returns an error if ANY decryption fails (Fix #3: all-or-nothing).
func (d *Decryptor) DecryptTestCase(tc *ossModels.TestCase) error {
	if tc == nil {
		return nil
	}
	var errs []error
	errs = append(errs, d.decryptHeaders(tc.HTTPReq.Header)...)
	errs = append(errs, d.decryptHeaders(tc.HTTPResp.Header)...)
	errs = append(errs, d.decryptStringMap(tc.HTTPReq.URLParams)...)

	// Decrypt form data values.
	for i := range tc.HTTPReq.Form {
		for j := range tc.HTTPReq.Form[i].Values {
			if IsEncrypted(tc.HTTPReq.Form[i].Values[j]) {
				decrypted, err := d.engine.Decrypt(tc.HTTPReq.Form[i].Values[j])
				if err != nil {
					errs = append(errs, fmt.Errorf("form field %q: %w", tc.HTTPReq.Form[i].Key, err))
				} else {
					tc.HTTPReq.Form[i].Values[j] = decrypted
				}
			}
		}
	}

	if body, err := d.decryptBodyStrict(tc.HTTPReq.Body); err != nil {
		errs = append(errs, err)
	} else {
		tc.HTTPReq.Body = body
	}
	if body, err := d.decryptBodyStrict(tc.HTTPResp.Body); err != nil {
		errs = append(errs, err)
	} else {
		tc.HTTPResp.Body = body
	}
	// URL query params: always attempt parse+decrypt since the sentinel may be
	// percent-encoded, unescaped, or partially encoded depending on how it was stored.
	if tc.HTTPReq.URL != "" {
		if parsed, err := url.Parse(tc.HTTPReq.URL); err == nil {
			query := parsed.Query()
			changed := false
			for key, values := range query {
				for i, v := range values {
					// Try raw value first, then URL-unescaped form.
					val := v
					if !IsEncrypted(val) {
						if unescaped, err := url.QueryUnescape(val); err == nil {
							val = unescaped
						}
					}
					if IsEncrypted(val) {
						decrypted, err := d.engine.Decrypt(val)
						if err != nil {
							errs = append(errs, fmt.Errorf("url param %q: %w", key, err))
						} else {
							query[key][i] = decrypted
							changed = true
						}
					}
				}
			}
			if changed {
				parsed.RawQuery = query.Encode()
				tc.HTTPReq.URL = parsed.String()
			}
		}
	}

	// gRPC headers + body
	errs = append(errs, d.decryptStringMap(tc.GrpcReq.Headers.OrdinaryHeaders)...)
	errs = append(errs, d.decryptStringMap(tc.GrpcReq.Headers.PseudoHeaders)...)
	errs = append(errs, d.decryptStringMap(tc.GrpcResp.Headers.OrdinaryHeaders)...)
	errs = append(errs, d.decryptStringMap(tc.GrpcResp.Headers.PseudoHeaders)...)
	errs = append(errs, d.decryptStringMap(tc.GrpcResp.Trailers.OrdinaryHeaders)...)
	if tc.GrpcReq.Body.DecodedData != "" {
		if body, err := d.decryptBodyStrict(tc.GrpcReq.Body.DecodedData); err != nil {
			errs = append(errs, err)
		} else {
			tc.GrpcReq.Body.DecodedData = body
		}
	}
	if tc.GrpcResp.Body.DecodedData != "" {
		if body, err := d.decryptBodyStrict(tc.GrpcResp.Body.DecodedData); err != nil {
			errs = append(errs, err)
		} else {
			tc.GrpcResp.Body.DecodedData = body
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("decryption failed for %d values in test case %q: first error: %w", len(errs), tc.Name, errs[0])
	}
	return nil
}

// DecryptMock decrypts all encrypted values in a mock in-place.
// Returns an error if ANY decryption fails (Fix #3: all-or-nothing).
func (d *Decryptor) DecryptMock(mock *ossModels.Mock) error {
	if mock == nil {
		return nil
	}
	var errs []error

	if mock.Spec.HTTPReq != nil {
		errs = append(errs, d.decryptHeaders(mock.Spec.HTTPReq.Header)...)
		errs = append(errs, d.decryptStringMap(mock.Spec.HTTPReq.URLParams)...)
		for i := range mock.Spec.HTTPReq.Form {
			for j := range mock.Spec.HTTPReq.Form[i].Values {
				if IsEncrypted(mock.Spec.HTTPReq.Form[i].Values[j]) {
					decrypted, err := d.engine.Decrypt(mock.Spec.HTTPReq.Form[i].Values[j])
					if err != nil {
						errs = append(errs, fmt.Errorf("mock form field %q: %w", mock.Spec.HTTPReq.Form[i].Key, err))
					} else {
						mock.Spec.HTTPReq.Form[i].Values[j] = decrypted
					}
				}
			}
		}
		if body, err := d.decryptBodyStrict(mock.Spec.HTTPReq.Body); err != nil {
			errs = append(errs, err)
		} else {
			mock.Spec.HTTPReq.Body = body
		}
		// URL-aware decryption for mock request URLs (same as test case).
		if mock.Spec.HTTPReq.URL != "" {
			if parsed, err := url.Parse(mock.Spec.HTTPReq.URL); err == nil {
				query := parsed.Query()
				changed := false
				for key, values := range query {
					for i, v := range values {
						val := v
						if !IsEncrypted(val) {
							if unescaped, err := url.QueryUnescape(val); err == nil {
								val = unescaped
							}
						}
						if IsEncrypted(val) {
							decrypted, err := d.engine.Decrypt(val)
							if err != nil {
								errs = append(errs, fmt.Errorf("mock url param %q: %w", key, err))
							} else {
								query[key][i] = decrypted
								changed = true
							}
						}
					}
				}
				if changed {
					parsed.RawQuery = query.Encode()
					mock.Spec.HTTPReq.URL = parsed.String()
				}
			}
			// Also try body-level decryption for path-embedded sentinels.
			if body, err := d.decryptBodyStrict(mock.Spec.HTTPReq.URL); err != nil {
				errs = append(errs, err)
			} else {
				mock.Spec.HTTPReq.URL = body
			}
		}
	}
	if mock.Spec.HTTPResp != nil {
		errs = append(errs, d.decryptHeaders(mock.Spec.HTTPResp.Header)...)
		if body, err := d.decryptBodyStrict(mock.Spec.HTTPResp.Body); err != nil {
			errs = append(errs, err)
		} else {
			mock.Spec.HTTPResp.Body = body
		}
	}
	// gRPC
	if mock.Spec.GRPCReq != nil {
		errs = append(errs, d.decryptStringMap(mock.Spec.GRPCReq.Headers.OrdinaryHeaders)...)
		if body, err := d.decryptBodyStrict(mock.Spec.GRPCReq.Body.DecodedData); err != nil {
			errs = append(errs, err)
		} else {
			mock.Spec.GRPCReq.Body.DecodedData = body
		}
	}
	if mock.Spec.GRPCResp != nil {
		errs = append(errs, d.decryptStringMap(mock.Spec.GRPCResp.Headers.OrdinaryHeaders)...)
		if body, err := d.decryptBodyStrict(mock.Spec.GRPCResp.Body.DecodedData); err != nil {
			errs = append(errs, err)
		} else {
			mock.Spec.GRPCResp.Body.DecodedData = body
		}
	}
	// Non-HTTP payloads
	errs = append(errs, decryptPayloadsStrict(d, mock.Spec.RedisRequests)...)
	errs = append(errs, decryptPayloadsStrict(d, mock.Spec.RedisResponses)...)
	errs = append(errs, decryptPayloadsStrict(d, mock.Spec.GenericRequests)...)
	errs = append(errs, decryptPayloadsStrict(d, mock.Spec.GenericResponses)...)

	if len(errs) > 0 {
		return fmt.Errorf("decryption failed for %d values in mock %q: first error: %w", len(errs), mock.Name, errs[0])
	}
	return nil
}

// decryptHeaders decrypts all encrypted header values, collecting errors.
func (d *Decryptor) decryptHeaders(headers map[string]string) []error {
	var errs []error
	for key, val := range headers {
		if IsEncrypted(val) {
			decrypted, err := d.engine.Decrypt(val)
			if err != nil {
				errs = append(errs, fmt.Errorf("header %q: %w", key, err))
			} else {
				headers[key] = decrypted
			}
		}
	}
	return errs
}

// decryptStringMap decrypts all encrypted values in a string map, collecting errors.
func (d *Decryptor) decryptStringMap(m map[string]string) []error {
	var errs []error
	for key, val := range m {
		if IsEncrypted(val) {
			decrypted, err := d.engine.Decrypt(val)
			if err != nil {
				errs = append(errs, fmt.Errorf("key %q: %w", key, err))
			} else {
				m[key] = decrypted
			}
		}
	}
	return errs
}

// decryptBodyStrict decrypts sentinels in a body, returning error on any failure.
func (d *Decryptor) decryptBodyStrict(body string) (string, error) {
	if !strings.Contains(body, EncryptedPrefix) {
		return body, nil
	}
	result := body
	for {
		idx := strings.Index(result, EncryptedPrefix)
		if idx == -1 {
			break
		}
		start := idx + len(EncryptedPrefix)
		end := start
		for end < len(result) && isBase64Char(result[end]) {
			end++
		}
		encrypted := result[idx:end]
		decrypted, err := d.engine.Decrypt(encrypted)
		if err != nil {
			return "", fmt.Errorf("body decrypt at offset %d: %w", idx, err)
		}
		result = result[:idx] + decrypted + result[end:]
	}
	return result, nil
}

func decryptPayloadsStrict(d *Decryptor, payloads []ossModels.Payload) []error {
	var errs []error
	for i := range payloads {
		for j := range payloads[i].Message {
			if body, err := d.decryptBodyStrict(payloads[i].Message[j].Data); err != nil {
				errs = append(errs, err)
			} else {
				payloads[i].Message[j].Data = body
			}
		}
	}
	return errs
}
