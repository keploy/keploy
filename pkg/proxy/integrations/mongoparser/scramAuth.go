package mongoparser

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.keploy.io/server/pkg/proxy/integrations/scram"
	"go.keploy.io/server/pkg/proxy/util"
	"go.uber.org/zap"
)

func isScramAuthRequest(actualRequestSections []string, logger *zap.Logger) bool {
	// Iterate over each section in the actual request sections
	for _, v := range actualRequestSections {
		// Extract the message from the section
		actualMsg, err := extractMsgFromSection(v)
		if err != nil {
			logger.Error("failed to extract the section of the recieved mongo request message", zap.Error(err), zap.Any("the section", v))
			return false
		}

		conversationId, _ := extractConversationId(actualMsg)
		// Check if the message is for starting the SASL (authentication) process
		if _, exists := actualMsg["saslStart"]; exists {
			logger.Debug("the recieved request is saslStart",
				zap.Any("OpMsg", actualMsg),
				zap.Any("conversationId", conversationId))
			return true
			// Check if the message is for final request of the SASL (authentication) process
		} else if _, exists := actualMsg["saslContinue"]; exists {
			logger.Debug("the recieved request is saslContinue",
				zap.Any("OpMsg", actualMsg),
				zap.Any("conversationId", conversationId),
			)
			return true
		}

	}
	return false
}

// authMessageMap stores the auth message from the saslStart request for the converstionIds. So, that
// it can be used in the saslContinue request to generate the new server proof
var authMessageMap map[string]string = map[string]string{}

// handleScramAuth handles the SCRAM authentication requests by generating the
// appropriate response string.
//
// Parameters:
//   - actualRequestSections: The sections from the recieved request received.
//   - expectedRequestSections: The sections that are recorded in the auth request.
//   - responseSection: The section to be used for the response.
//   - logger: The logging instance for recording activities and errors.
//
// Returns:
//   - The generated response string.
//   - A boolean indicating if the processing was successful.
//   - An error, if any, that occurred during processing.
func handleScramAuth(actualRequestSections, expectedRequestSections []string, responseSection string, logger *zap.Logger) (string, bool, error) {
	// Iterate over each section in the actual request sections
	for i, v := range actualRequestSections {
		// single document do not uses section sequence for SCRAM auth
		if !strings.HasPrefix(v, "{ SectionSingle msg:") {
			continue
		}

		// Extract the message from the section
		actualMsg, err := extractMsgFromSection(v)
		if err != nil {
			logger.Error("failed to extract the section of the recieved mongo request message", zap.Error(err))
			return "", false, err
		}

		// Check if the message is for starting the SASL (authentication) process
		if _, exists := actualMsg["saslStart"]; exists {
			mechanism, exists := actualMsg["mechanism"]
			// Check the authentication mechanism used and ensure it contains "SCRAM"
			if mechanism, ok := mechanism.(string); exists && ok && strings.Contains(mechanism, "SCRAM") {
				if _, exists := actualMsg["payload"]; exists {
					return handleSaslStart(i, actualMsg, expectedRequestSections, responseSection, logger)
				}
			}
			// Check if the message is for final request of the SASL (authentication) process
		} else if _, exists := actualMsg["saslContinue"]; exists {
			if _, exists := actualMsg["payload"]; exists {
				return handleSaslContinue(actualMsg, responseSection, logger)
			}
		}
	}
	return "", false, nil
}

// extractAuthPayload extracts the base64 authentication payload from a given data structure.
//
// Parameters:
//   - data: The interface{} that should represent a nested map with expected keys.
//
// Returns:
//   - The extracted base64 string from the nested map structure.
//   - An error if the data doesn't have the expected nested structure or if the expected keys are missing.
func extractAuthPayload(data interface{}) (string, error) {
	// Top-level map
	topMap, ok := data.(map[string]interface{})
	if !ok {
		return "", errors.New("expected top-level data to be a map")
	}

	// Payload map
	payload, ok := topMap["payload"].(map[string]interface{})
	if !ok {
		return "", errors.New("expected 'payload' to be a map")
	}

	// $binary map
	binaryMap, ok := payload["$binary"].(map[string]interface{})
	if !ok {
		return "", errors.New("expected '$binary' to be a map")
	}

	// Base64 string
	base64Str, ok := binaryMap["base64"].(string)
	if !ok {
		return "", errors.New("expected 'base64' to be a string")
	}

	return base64Str, nil
}

// extractConversationId extracts the 'conversationId' from a given data structure. Example: {"conversationId":{"$numberInt":"113"}}
//
// Parameters:
//   - data: The interface{} that should represent a map containing the key 'conversationId'.
//
// Returns:
//   - The extracted conversationId as a string.
//   - An error if the expected 'conversationId' structure isn't present or if other expected keys are missing.
func extractConversationId(data interface{}) (string, error) {
	// Top-level map
	topMap, ok := data.(map[string]interface{})
	if !ok {
		return "", errors.New("expected top-level data to be a map")
	}

	conversationId, exists := topMap["conversationId"]
	if !exists {
		return "", errors.New("'conversationId' not found")
	}

	// conversationId map
	conversationIdMap, ok := conversationId.(map[string]interface{})
	if !ok {
		return "", errors.New("expected 'conversationId' to be a map")
	}

	// Check presence of "$numberInt"
	num, exists := conversationIdMap["$numberInt"]
	if !exists {
		return "", errors.New("'$numberInt' not found")
	}
	numberIntStr, present := num.(string)
	if !present {
		return "", errors.New("expected '$numberInt' to be a string")
	}

	return numberIntStr, nil
}

// updateConversationId updates the 'conversationId' in a given data structure. Example: {"conversationId":{"$numberInt":"113"}}
func updateConversationId(actualMsg map[string]interface{}, newConversationId int) (map[string]interface{}, error) {
	// Check if conversationId exists and is a map
	conversationId, exists := actualMsg["conversationId"]
	if !exists {
		return actualMsg, errors.New("'conversationId' not found")
	}

	conversationIdMap, ok := conversationId.(map[string]interface{})
	if !ok {
		return actualMsg, errors.New("expected 'conversationId' to be a map")
	}

	// Update the "$numberInt" field with the new value
	conversationIdMap["$numberInt"] = fmt.Sprintf("%d", newConversationId)
	actualMsg["conversationId"] = conversationIdMap
	return actualMsg, nil
}


// decodeBase64Str is a function variable that wraps the standard Base64 decoding method,
// taking a Base64 encoded string and returning its decoded byte array and any error.
var decodeBase64Str func(s string) ([]byte, error) = base64.StdEncoding.DecodeString

// extractMsgFromSection decodes an OP_MSG section string, and then
// unmarshals the resulting string into a map.
//
// Parameters:
//   - section: The OP_MSG section string to decode and unmarshal.
//
// Returns:
//   - A map containing the key-value pairs from the unmarshaled section.
//   - An error if there's an issue during decoding or unmarshaling.
func extractMsgFromSection(section string) (map[string]interface{}, error) {
	sectionStr, err := extractSectionSingle(section)
	if err != nil {
		return nil, err
	}

	var result map[string]interface{}
	err = json.Unmarshal([]byte(sectionStr), &result)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func handleSaslStart(i int, actualMsg map[string]interface{}, expectedRequestSections []string, responseSection string, logger *zap.Logger) (string, bool, error) {
	actualReqPayload, err := extractAuthPayload(actualMsg)
	if err != nil {
		logger.Error("failed to fetch the payload from the recieved mongo request", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprint("the payload of the recieved request: ", actualReqPayload))

	// Decode the base64 encoded payload of the recieved mongo request
	decodedActualReqPayload, err := decodeBase64Str(actualReqPayload)
	if err != nil {
		logger.Error("Error decoding the recieved payload base64 string:", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprint("the decoded payload of the actual for the saslstart: ", (string)(decodedActualReqPayload)))

	// check to ensure that the matched recorded mongo request contains the auth payload for SCRAM
	if len(expectedRequestSections) < i+1 {
		err = errors.New("unrecorded message sections for the recieved auth request")
		logger.Error("failed to match the message section payload", zap.Error(err))
		return "", false, err
	}

	expectedMsg, err := extractMsgFromSection(expectedRequestSections[i])
	if err != nil {
		logger.Error("failed to extract the section of the recorded mongo request message", zap.Error(err))
		return "", false, err
	}

	expectedReqPayload, err := extractAuthPayload(expectedMsg)
	if err != nil {
		logger.Error("failed to fetch the payload from the recorded mongo request", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprint("the payload of the recorded request: ", expectedReqPayload))

	// Decode the base64 encoded payload of the recorded mongo request
	decodedExpectedReqPayload, err := decodeBase64Str(expectedReqPayload)
	if err != nil {
		logger.Error("Error decoding the recorded request payload base64 string:", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprint("the decoded payload of the expected for the saslstart: ", (string)(decodedExpectedReqPayload)))

	// the payload of the recorded first response of SCRAM authentication
	var responseMsg map[string]interface{}

	err = json.Unmarshal([]byte(responseSection), &responseMsg)
	if err != nil {
		logger.Error("failed to unmarshal string document of OpReply", zap.Error(err))
		return "", false, err
	}
	responsePayload, err := extractAuthPayload(responseMsg)
	if err != nil {
		logger.Error("failed to fetch the payload from the recorded mongo response", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprint("the payload of the recorded response: ", responsePayload))

	// Decode the base64 encoded payload of the recorded mongo response
	decodedResponsePayload, err := decodeBase64Str(responsePayload)
	if err != nil {
		logger.Error("Error decoding the recorded response payload base64 string:", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprint("the decoded payload of the repsonse for the saslstart: ", (string)(decodedResponsePayload)))

	// Generate the first response for the saslStart request by
	// replacing the old client nonce with new client nonce
	newFirstAuthResponse, err := scram.GenerateServerFirstMessage(decodedExpectedReqPayload, decodedActualReqPayload, decodedResponsePayload, logger)
	if err != nil {
		return "", false, err
	}
	logger.Debug("after replacing the new client nonce in auth response", zap.String("first response", newFirstAuthResponse))
	// replace the payload with new first response auth
	responseMsg["payload"].(map[string]interface{})["$binary"].(map[string]interface{})["base64"] = base64.StdEncoding.EncodeToString([]byte(newFirstAuthResponse))
	responseMsg, err = updateConversationId(responseMsg, int(util.GetNextID()))
	if err != nil {
		logger.Error("failed to update conversationId in the sasl start auth message", zap.Error(err))
		return "", false, err
	}

	// fetch the conversation id
	conversationId, err := extractConversationId(responseMsg)
	if err != nil {
		logger.Error("failed to fetch the conversationId for the SCRAM auth from the recorded first response", zap.Error(err))
		return "", false, err
	}
	logger.Debug("fetch the conversationId for the SCRAM authentication", zap.String("cid", conversationId))
	// generate the auth message from the recieved first request and recorded first response
	authMessage := scram.GenerateAuthMessage(string(decodedActualReqPayload), newFirstAuthResponse, logger)
	// store the auth message in the global map for the conversationId
	authMessageMap[conversationId] = authMessage
	logger.Debug("genrate the new auth message for the recieved auth request", zap.String("msg", authMessage))

	// marshal the new first response for the SCRAM authentication
	newAuthResponse, err := json.Marshal(responseMsg)
	if err != nil {
		logger.Error("failed to marshal the first auth response for SCRAM", zap.Error(err))
		return "", false, err
	}
	return string(newAuthResponse), true, nil
}

// handleSaslContinue processes a SASL continuation message, updates the payload with
// the new verifier, which is prepared by the new auth message.
//
// Parameters:
//   - actualMsg: The actual message map from the client.
//   - responseSection: The section string to be used for the response.
//   - logger: The logging instance for recording activities and errors.
//
// Returns:
//   - The updated response section string.
//   - A boolean indicating if the processing was successful.
//   - An error, if any, that occurred during processing.
func handleSaslContinue(actualMsg map[string]interface{}, responseSection string, logger *zap.Logger) (string, bool, error) {
	var responseMsg map[string]interface{}

	err := json.Unmarshal([]byte(responseSection), &responseMsg)
	if err != nil {
		logger.Error("failed to unmarshal string document of second auth response for SCRAM", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprintf("the recorded OpMsg section: %v", responseMsg))

	responsePayload, err := extractAuthPayload(responseMsg)
	if err != nil {
		logger.Error("failed to fetch the payload from the recorded mongo response", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprint("the payload of the recorded second response of SCRAM: ", responsePayload))

	decodedResponsePayload, err := decodeBase64Str(responsePayload)
	if err != nil {
		logger.Error("Error decoding the recorded saslContinue response payload base64 string:", zap.Error(err))
		return "", false, err
	}
	logger.Debug(fmt.Sprint("the decoded payload of the repsonse for the saslContinue: ", (string)(decodedResponsePayload)))

	fields := strings.Split(string(decodedResponsePayload), ",")
	verifier, err := parseFieldBase64(fields[0], "v")
	if err != nil {
		logger.Error("failed to parse the verifier of final response message", zap.Error(err))
		return "", false, err
	}
	logger.Debug("the recorded verifier of the auth request", zap.Any("verifier/server-signature", string(verifier)))

	// fetch the conversation id
	conversationId, err := extractConversationId(actualMsg)
	if err != nil {
		logger.Error("failed to fetch the conversationId for the SCRAM auth from the recorded final response", zap.Error(err))
		return "", false, err
	}
	logger.Debug("fetched conversationId for the SCRAM authentication", zap.String("cid", conversationId))

	salt := ""
	itr := 0
	// get the authMessage from the saslStart conversation. Since, saslContinue have the same conversationId
	authMsg := authMessageMap[conversationId]

	// get the salt and iteration from the authMessage to generate salted password
	fields = strings.Split(authMsg, ",")
	for _, part := range fields {
		if strings.HasPrefix(part, "s=") {
			// Split based on "=" and get the value of "s"
			saltByt, err := decodeBase64Str(strings.TrimPrefix(part, "s="))
			if err != nil {
				logger.Error("failed to convert the string into integer", zap.Error(err))
				return "", false, err
			}
			salt = string(saltByt)
		}
		if strings.HasPrefix(part, "i=") {
			// Split based on "=" and get the value of "i"
			itr, err = strconv.Atoi(strings.Split(part, "=")[1])
			if err != nil {
				logger.Error("failed to convert the string into integer", zap.Error(err))
				return "", false, err
			}
		}
	}
	// Since, the server proof is the signature generated by the authMessage and salted password.
	// So, need to return the new server proof according to the new authMessage which is different from the recorded.
	newVerifier, err := scram.GenerateServerFinalMessage(authMessageMap[conversationId], "SCRAM-SHA-1", password, salt, itr, logger)
	if err != nil {
		logger.Error("failed to get the new server proof", zap.Error(err))
		return "", false, err
	}

	// update the payload of the mongo response for the authentication
	responseMsg["payload"].(map[string]interface{})["$binary"].(map[string]interface{})["base64"] = base64.StdEncoding.EncodeToString([]byte("v=" + newVerifier))
	byt, err := json.Marshal(responseMsg)
	if err != nil {
		logger.Error("failed to marshal the updated string document of OpReply", zap.Error(err))
		return "", false, err
	}
	responseSection = string(byt)
	return responseSection, true, nil
}

func parseField(s, k string) (string, error) {
	t := strings.TrimPrefix(s, k+"=")
	if t == s {
		return "", fmt.Errorf("error parsing '%s' for field '%s'", s, k)
	}
	return t, nil
}

func parseFieldBase64(s, k string) ([]byte, error) {
	raw, err := parseField(s, k)
	if err != nil {
		return nil, err
	}

	dec, err := decodeBase64Str(raw)
	if err != nil {
		return nil, err
	}

	return dec, nil
}
