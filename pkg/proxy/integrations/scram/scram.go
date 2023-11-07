package scram

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/xdg-go/pbkdf2"
	"github.com/xdg-go/scram"
	"github.com/xdg-go/stringprep"
	"go.uber.org/zap"
)

// GenerateServerFinalMessage generates the server's final message (i.e., the server proof)
// for SCRAM authentication, using a default password and given authentication message, mechanism, salt, iteration count.
func GenerateServerFinalMessage(authMessage, mechanism, password, salt string, itr int, logger *zap.Logger) (string, error) {
	var (
		// Declare a variable to hold the hash generation function based on the chosen mechanism.
		hashGen scram.HashGeneratorFcn
		// normalised password is used in the salted password
		passwordDigest string
	)

	username, err := extractUsername(authMessage)
	if err != nil {
		return "", err
	}

	// Switch based on the provided mechanism to determine the hash function to be used.
	switch mechanism {
	case "SCRAM-SHA-1":
		hashGen = scram.SHA1
		passwordDigest = mongoPasswordDigest(username, password)
	case "SCRAM-SHA-256":
		hashGen = scram.SHA256
		passwordDigest, err = stringprep.SASLprep.Prepare(password)
		if err != nil {
			return "", fmt.Errorf("error SASLprepping password for SCRAM-SHA-256 with password: %s. error: %v", password, err.Error())
		}
	default:
		// If the mechanism isn't supported, return an error.
		return "", errors.New("unsupported authentication mechanism by keploy")
	}

	// Get the hash function instance based on the determined generator.
	h := hashGen()

	// Compute the salted password using the PBKDF2 function with the provided salt and iteration count.
	// It uses the given password. This is the key derivation step.
	logger.Debug("the input for generating the salted password", zap.Any("normalised password", passwordDigest), zap.Any("salt", salt), zap.Any("iteration", itr), zap.Any("hash size", h.Size()), zap.Any("mechanism", mechanism))
	saltedPassword := pbkdf2.Key([]byte(passwordDigest), []byte(salt), itr, h.Size(), hashGen)
	logger.Debug("after generating the salted password", zap.Any("salted password", saltedPassword))

	// Compute the server key using HMAC with the derived salted password and the string "Server Key".
	serverKey := computeHMAC(hashGen, saltedPassword, []byte("Server Key"))
	logger.Debug("generating the server using the salted password", zap.Any("server key", serverKey))

	// Compute the server signature (server proof) using HMAC with the server key and the provided authMessage.
	serverSignature := computeHMAC(hashGen, serverKey, []byte(authMessage))
	logger.Debug("the new server proof for the second auth request", zap.Any("server signature", base64.StdEncoding.EncodeToString(serverSignature)))

	return base64.StdEncoding.EncodeToString(serverSignature), nil
}

// GenerateServerFirstMessage generates the server's first response message for SCRAM authentication.
// It replaces the expected nonce from the recorded request with the actual nonce from the received request.
//
// Parameters:
// - recordedRequestMsg: The byte slice containing the recorded client's first message.
// - recievedRequestMsg: The byte slice containing the received client's first message.
// - firstResponseMsg: The byte slice containing the server's initial response message.
// - logger: An instance of a logger from the zap package.
//
// Returns:
// - A modified server's first response message with the nonce replaced.
// - An error if nonce extraction or replacement fails.
func GenerateServerFirstMessage(recordedRequestMsg, recievedRequestMsg, firstResponseMsg []byte, logger *zap.Logger) (string, error) {
	expectedNonce, err := extractClientNonce(string(recordedRequestMsg))
	if err != nil {
		logger.Error("failed to extract the client nonce from the recorded first message", zap.Error(err))
		return "", err
	}
	actualNonce, err := extractClientNonce(string(recievedRequestMsg))
	if err != nil {
		logger.Error("failed to extract the client nonce from the recieved first message", zap.Error(err))
		return "", err
	}
	// Since, the nonce are randomlly generated string. so, each session have unique nonce.
	// Thus, the mocked server response should be updated according to the current nonce
	return strings.Replace(string(firstResponseMsg), expectedNonce, actualNonce, -1), nil
}

// GenerateAuthMessage creates an authentication message based on the initial
// client request and the server's first response. The function extracts the GS2
// header and the client's nonce from the provided strings and then concatenates
// them to form the complete authentication message.
//
// Parameters:
//   - firstRequest: The initial request string from the client.
//   - firstResponse: The server's first response string.
//   - logger: An instance of a logger for logging errors and activities.
//
// Returns:
//   - A string representing the complete authentication message. If there's an
//     error during extraction or message creation, the function logs the error
//     and returns an empty string.
func GenerateAuthMessage(firstRequest, firstResponse string, logger *zap.Logger) string {
	gs2, err := extractAuthId(firstRequest)
	if err != nil {
		logger.Error("failed to extract the client gs2 header from the recieved first message", zap.Error(err))
		return ""
	}
	authMsg := firstRequest[len(gs2):] + "," + firstResponse + ","
	nonce, err := extractClientNonce(firstResponse)
	if err != nil {
		logger.Error("failed to extract the client nonce from the recorded first message", zap.Error(err))
		return ""
	}

	authMsg += fmt.Sprintf(
		"c=%s,r=%s",
		base64.StdEncoding.EncodeToString([]byte(gs2)),
		nonce,
	)

	return authMsg
}
