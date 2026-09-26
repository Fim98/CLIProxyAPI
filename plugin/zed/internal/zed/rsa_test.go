package zed

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"testing"
)

func TestGenerateLoginKeypairPublicKeyRoundTrip(t *testing.T) {
	publicKey, privateKey, errGenerate := GenerateLoginKeypair()
	if errGenerate != nil {
		t.Fatalf("GenerateLoginKeypair() error = %v", errGenerate)
	}
	der, errDecode := base64.URLEncoding.DecodeString(publicKey)
	if errDecode != nil {
		t.Fatalf("public key is not base64 URL-safe: %v", errDecode)
	}
	parsed, errParse := x509.ParsePKCS1PublicKey(der)
	if errParse != nil {
		t.Fatalf("public key is not PKCS#1 DER: %v", errParse)
	}
	if parsed.N.Cmp(privateKey.PublicKey.N) != 0 {
		t.Fatalf("public key does not match private key")
	}
}

func TestDecryptAccessTokenOAEP(t *testing.T) {
	_, privateKey, errGenerate := GenerateLoginKeypair()
	if errGenerate != nil {
		t.Fatalf("GenerateLoginKeypair() error = %v", errGenerate)
	}
	plaintext := "zed-access-token-value"
	ciphertext, errEncrypt := rsa.EncryptOAEP(sha256.New(), rand.Reader, &privateKey.PublicKey, []byte(plaintext), nil)
	if errEncrypt != nil {
		t.Fatalf("encrypt: %v", errEncrypt)
	}
	decrypted, errDecrypt := DecryptAccessToken(privateKey, base64.URLEncoding.EncodeToString(ciphertext))
	if errDecrypt != nil {
		t.Fatalf("DecryptAccessToken() error = %v", errDecrypt)
	}
	if decrypted != plaintext {
		t.Fatalf("DecryptAccessToken() = %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptAccessTokenPKCS1v15Fallback(t *testing.T) {
	_, privateKey, errGenerate := GenerateLoginKeypair()
	if errGenerate != nil {
		t.Fatalf("GenerateLoginKeypair() error = %v", errGenerate)
	}
	plaintext := "legacy-token"
	ciphertext, errEncrypt := rsa.EncryptPKCS1v15(rand.Reader, &privateKey.PublicKey, []byte(plaintext))
	if errEncrypt != nil {
		t.Fatalf("encrypt: %v", errEncrypt)
	}
	decrypted, errDecrypt := DecryptAccessToken(privateKey, base64.URLEncoding.EncodeToString(ciphertext))
	if errDecrypt != nil {
		t.Fatalf("DecryptAccessToken() error = %v", errDecrypt)
	}
	if decrypted != plaintext {
		t.Fatalf("DecryptAccessToken() = %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptAccessTokenRejectsGarbage(t *testing.T) {
	_, privateKey, errGenerate := GenerateLoginKeypair()
	if errGenerate != nil {
		t.Fatalf("GenerateLoginKeypair() error = %v", errGenerate)
	}
	if _, errDecrypt := DecryptAccessToken(privateKey, "not-base64!"); errDecrypt == nil {
		t.Fatal("DecryptAccessToken() accepted invalid base64")
	}
}
