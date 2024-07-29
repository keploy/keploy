//go:build linux

package mysql

// MySQL authentication methods
const (
	NativePassword      = "mysql_native_password"
	CachingSha2Password = "caching_sha2_password"
	Sha256Password      = "sha256_password"
)

// Some constants for MySQL
const (
	EncryptedPassword = "encrypted_password"
)
