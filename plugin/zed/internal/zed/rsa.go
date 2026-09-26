package zed

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"strings"
)

// GenerateLoginKeypair creates the RSA keypair for one browser sign-in,
// mirroring crates/rpc/src/auth.rs keypair(): a 2048-bit RSA key whose public
// half travels as base64(URL_SAFE) of the PKCS#1 DER encoding.
func GenerateLoginKeypair() (string, *rsa.PrivateKey, error) {
	privateKey, errGenerate := rsa.GenerateKey(rand.Reader, 2048)
	if errGenerate != nil {
		return "", nil, fmt.Errorf("generate Zed login keypair: %w", errGenerate)
	}
	publicDER := x509.MarshalPKCS1PublicKey(&privateKey.PublicKey)
	publicKey := base64.URLEncoding.EncodeToString(publicDER)
	return publicKey, privateKey, nil
}

// DecryptAccessToken decrypts the access_token query parameter the Zed web app
// appends to the loopback callback. The server encrypts with OAEP-SHA256; older
// deployments used PKCS#1 v1.5, so both are attempted, matching decrypt_string
// in crates/rpc/src/auth.rs.
func DecryptAccessToken(privateKey *rsa.PrivateKey, encrypted string) (string, error) {
	ciphertext, errDecode := decodeBase64URL(encrypted)
	if errDecode != nil {
		return "", fmt.Errorf("decode Zed callback access token: %w", errDecode)
	}
	plaintext, errOAEP := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, ciphertext, nil)
	if errOAEP == nil {
		return string(plaintext), nil
	}
	plaintext, errPKCS1 := rsa.DecryptPKCS1v15(rand.Reader, privateKey, ciphertext)
	if errPKCS1 != nil {
		return "", fmt.Errorf("decrypt Zed callback access token: %w", errOAEP)
	}
	return string(plaintext), nil
}

func decodeBase64URL(value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	if decoded, errDecode := base64.URLEncoding.DecodeString(trimmed); errDecode == nil {
		return decoded, nil
	}
	return base64.RawURLEncoding.DecodeString(trimmed)
}
