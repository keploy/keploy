package mongo

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
	"go.mongodb.org/mongo-driver/x/mongo/driver"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

type Message struct {
	Wm []byte
	Op Operation
}

type TransactionDetails struct {
	LsID               []byte
	TxnNumber          int64
	IsStartTransaction bool
}

type Operation interface {
	fmt.Stringer
	OpCode() wiremessage.OpCode
	Encode(responseTo, requestId int32) []byte
	IsIsMaster() bool
	IsIsAdminDB() bool
	CursorID() (cursorID int64, ok bool)
	RequestID() int32
	Error() error
	Unacknowledged() bool
	CommandAndCollection() (Command, string)
	TransactionDetails() *TransactionDetails
}

var lOgger *zap.Logger

// see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/operation.go#L1361-L1426
// func Decode(wm []byte) (Operation, int32, int32, int32, int32, error) {
func Decode(wm []byte, logger *zap.Logger) (Operation, models.MongoHeader, interface{}, error) {
	lOgger = logger
	wmLength := len(wm)
	length, reqID, responseTo, opCode, wmBody, ok := wiremessage.ReadHeader(wm)
	messageHeader := models.MongoHeader{
		Length:     length,
		RequestID:  reqID,
		ResponseTo: responseTo,
		Opcode:     wiremessage.OpCode(opCode),
	}
	logger.Debug(fmt.Sprintf("the mongo msg header: %v", messageHeader))
	if !ok || int(length) > wmLength {
		return nil, messageHeader, &models.MongoOpMessage{}, errors.New("malformed wire message: insufficient bytes")
	}

	var (
		op       Operation
		err      error
		mongoMsg interface{}
	)
	// var err error
	switch opCode {
	case wiremessage.OpQuery:
		op, err = decodeQuery(reqID, wmBody)
		if err != nil {
			return nil, messageHeader, mongoMsg, err
		}
		jsonBytes, err := bson.MarshalExtJSON(op.(*opQuery).query, true, false)
		if err != nil {
			return nil, messageHeader, &models.MongoOpMessage{}, fmt.Errorf("malformed bson document: %v", err.Error())
		}
		jsonString := string(jsonBytes)

		mongoMsg = &models.MongoOpQuery{
			Flags:                int32(op.(*opQuery).flags),
			FullCollectionName:   op.(*opQuery).fullCollectionName,
			NumberToSkip:         op.(*opQuery).numberToSkip,
			NumberToReturn:       op.(*opQuery).numberToReturn,
			Query:                jsonString,
			ReturnFieldsSelector: op.(*opQuery).returnFieldsSelector.String(),
		}
	case wiremessage.OpMsg:
		op, err = decodeMsg(reqID, wmBody)
		if err != nil {
			return nil, messageHeader, mongoMsg, err
		}
		var sections []string
		for _, section := range op.(*opMsg).sections {
			sections = append(sections, section.String())
		}
		mongoMsg = &models.MongoOpMessage{
			FlagBits: int(op.(*opMsg).flags),
			Sections: sections,
			Checksum: int(op.(*opMsg).checksum),
		}
	case wiremessage.OpReply:
		op, err = decodeReply(reqID, wmBody)
		if err != nil {
			return nil, messageHeader, mongoMsg, err
		}
		replyDocs := []string{}
		for _, v := range op.(*opReply).documents {
			jsonBytes, err := bson.MarshalExtJSON(v, true, false)
			if err != nil {
				return nil, messageHeader, &models.MongoOpMessage{}, fmt.Errorf("malformed bson document: %v", err.Error())
			}
			jsonString := string(jsonBytes)
			replyDocs = append(replyDocs, jsonString)
		}
		mongoMsg = &models.MongoOpReply{
			ResponseFlags:  int32(op.(*opReply).flags),
			CursorID:       op.(*opReply).cursorID,
			StartingFrom:   op.(*opReply).startingFrom,
			NumberReturned: op.(*opReply).numReturned,
			Documents:      replyDocs,
		}
	default:
		op = &opUnknown{
			opCode: opCode,
			reqID:  reqID,
			wm:     wm,
		}
	}
	if err != nil {
		return nil, messageHeader, mongoMsg, err
	}
	logger.Debug(fmt.Sprintf("the decoded string for the wiremessage: %v", op.String()))
	return op, messageHeader, mongoMsg, nil
}

type opUnknown struct {
	opCode wiremessage.OpCode
	reqID  int32
	wm     []byte
}

func (o *opUnknown) IsIsAdminDB() bool {
	return false
}

func (o *opUnknown) TransactionDetails() *TransactionDetails {
	return nil
}

func (o *opUnknown) OpCode() wiremessage.OpCode {
	return o.opCode
}

func (o *opUnknown) Encode(responseTo, requestId int32) []byte {
	return o.wm
}

func (o *opUnknown) IsIsMaster() bool {
	return false
}

func (o *opUnknown) CursorID() (cursorID int64, ok bool) {
	return 0, false
}

func (o *opUnknown) RequestID() int32 {
	return o.reqID
}

func (o *opUnknown) Error() error {
	return nil
}

func (o *opUnknown) Unacknowledged() bool {
	return false
}

func (o *opUnknown) CommandAndCollection() (Command, string) {
	return Unknown, ""
}

func (o *opUnknown) String() string {
	return fmt.Sprintf("{ OpUnknown opCode: %d, wm: %s }", o.opCode, o.wm)
}

// https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#wire-op-query
type opQuery struct {
	reqID                int32
	flags                wiremessage.QueryFlag
	fullCollectionName   string
	numberToSkip         int32
	numberToReturn       int32
	query                bsoncore.Document
	returnFieldsSelector bsoncore.Document
}

func (opQuery) IsIsAdminDB() bool {
	return false
}

func (q *opQuery) TransactionDetails() *TransactionDetails {
	return nil
}

// see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/topology/server_test.go#L968-L1003
func decodeQuery(reqID int32, wm []byte) (*opQuery, error) {
	var ok bool
	q := opQuery{
		reqID: reqID,
	}

	q.flags, wm, ok = wiremessage.ReadQueryFlags(wm)
	if !ok {
		return nil, errors.New("malformed query message: missing OP_QUERY flags")
	}

	q.fullCollectionName, wm, ok = wiremessage.ReadQueryFullCollectionName(wm)
	if !ok {
		return nil, errors.New("malformed query message: full collection name")
	}

	q.numberToSkip, wm, ok = wiremessage.ReadQueryNumberToSkip(wm)
	if !ok {
		return nil, errors.New("malformed query message: number to skip")
	}

	q.numberToReturn, wm, ok = wiremessage.ReadQueryNumberToReturn(wm)
	if !ok {
		return nil, errors.New("malformed query message: number to return")
	}

	q.query, wm, ok = wiremessage.ReadQueryQuery(wm)
	if !ok {
		return nil, errors.New("malformed query message: query document")
	}

	if len(wm) > 0 {
		q.returnFieldsSelector, _, ok = wiremessage.ReadQueryReturnFieldsSelector(wm)
		if !ok {
			return nil, errors.New("malformed query message: return fields selector")
		}
	}

	return &q, nil
}

func (q *opQuery) OpCode() wiremessage.OpCode {
	return wiremessage.OpQuery
}

// see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/operation_legacy.go#L179-L189
func (q *opQuery) Encode(responseTo, requestId int32) []byte {
	var buffer []byte
	idx, buffer := wiremessage.AppendHeaderStart(buffer, 0, responseTo, wiremessage.OpQuery)
	buffer = wiremessage.AppendQueryFlags(buffer, q.flags)
	buffer = wiremessage.AppendQueryFullCollectionName(buffer, q.fullCollectionName)
	buffer = wiremessage.AppendQueryNumberToSkip(buffer, q.numberToSkip)
	buffer = wiremessage.AppendQueryNumberToReturn(buffer, q.numberToReturn)
	buffer = append(buffer, q.query...)
	if len(q.returnFieldsSelector) != 0 {
		// returnFieldsSelector is optional
		buffer = append(buffer, q.returnFieldsSelector...)
	}
	buffer = bsoncore.UpdateLength(buffer, idx, int32(len(buffer[idx:])))
	return buffer
}

func (q *opQuery) CursorID() (cursorID int64, ok bool) {
	return q.query.Lookup("getMore").Int64OK()
}

func (q *opQuery) RequestID() int32 {
	return q.reqID
}

func (q *opQuery) IsIsMaster() bool {
	if q.fullCollectionName != "admin.$cmd" {
		return false
	}
	return IsIsMasterDoc(q.query)
}

func (q *opQuery) Error() error {
	return nil
}

func (q *opQuery) Unacknowledged() bool {
	return false
}

func (q *opQuery) CommandAndCollection() (Command, string) {
	return Find, q.fullCollectionName
}

func (q *opQuery) String() string {
	return fmt.Sprintf("{ OpQuery flags: %s, fullCollectionName: %s, numberToSkip: %d, numberToReturn: %d, query: %s, returnFieldsSelector: %s }", q.flags.String(), q.fullCollectionName, q.numberToSkip, q.numberToReturn, q.query.String(), q.returnFieldsSelector.String())
}

// https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#op-msg
type opMsg struct {
	reqID    int32
	flags    wiremessage.MsgFlag
	sections []opMsgSection
	checksum uint32
}

type opMsgSection interface {
	fmt.Stringer
	cursorID() (cursorID int64, ok bool)
	isIsMaster() bool
	isDbAdmin() bool
	append(buffer []byte) []byte
	commandAndCollection() (Command, string)
}

type opMsgSectionSingle struct {
	msg bsoncore.Document
}

func (o *opMsgSectionSingle) cursorID() (cursorID int64, ok bool) {
	if getMore, ok := o.msg.Lookup("getMore").Int64OK(); ok {
		return getMore, ok
	}
	return o.msg.Lookup("cursor", "id").Int64OK()
}

func (o *opMsgSectionSingle) isIsMaster() bool {
	if db, ok := o.msg.Lookup("$db").StringValueOK(); ok && db == "admin" {
		return IsIsMasterDoc(o.msg)
	}
	return false
}

func (o *opMsgSectionSingle) isDbAdmin() bool {
	if db, ok := o.msg.Lookup("$db").StringValueOK(); ok && db == "admin" {
		return true
	}
	return false
}

func (o *opMsgSectionSingle) append(buffer []byte) []byte {
	buffer = wiremessage.AppendMsgSectionType(buffer, wiremessage.SingleDocument)
	return append(buffer, o.msg...)
}

func (o *opMsgSectionSingle) commandAndCollection() (Command, string) {
	return CommandAndCollection(o.msg)
}

func (o *opMsgSectionSingle) String() string {
	jsonBytes, err := bson.MarshalExtJSON(o.msg, true, false)
	if err != nil {
		return ""
	}
	jsonString := string(jsonBytes)

	return fmt.Sprintf("{ SectionSingle msg: %s }", jsonString)
}

type opMsgSectionSequence struct {
	identifier string
	msgs       []bsoncore.Document
}

func (o *opMsgSectionSequence) cursorID() (cursorID int64, ok bool) {
	// assume no cursor IDs are returned in OP_MSG document sequences
	return 0, false
}

func (o *opMsgSectionSequence) isIsMaster() bool {
	return false
}
func (o *opMsgSectionSequence) isDbAdmin() bool {
	res := true
	for _, v := range o.msgs {
		if db, ok := v.Lookup("$db").StringValueOK(); !ok || db != "admin" {
			res = false
			break
		}
	}
	return res
}

func (o *opMsgSectionSequence) append(buffer []byte) []byte {
	buffer = wiremessage.AppendMsgSectionType(buffer, wiremessage.DocumentSequence)

	length := int32(len(o.identifier) + 5)
	for _, msg := range o.msgs {
		length += int32(len(msg))
	}

	buffer = appendi32(buffer, length)
	buffer = appendCString(buffer, o.identifier)
	for _, msg := range o.msgs {
		buffer = append(buffer, msg...)
	}

	return buffer
}

func (o *opMsgSectionSequence) commandAndCollection() (Command, string) {
	return Unknown, ""
}

func (o *opMsgSectionSequence) String() string {
	var msgs []string
	for _, msg := range o.msgs {
		jsonBytes, err := bson.MarshalExtJSON(msg, true, false)
		if err != nil {
			return ""
		}
		jsonString := string(jsonBytes)
		msgs = append(msgs, jsonString)
	}
	return fmt.Sprintf("{ SectionSingle identifier: %s , msgs: [ %s ] }", o.identifier, strings.Join(msgs, ", "))
}

func decodeOpMsgSectionSequence(section string) (string, string, error) {
	var identifier, message = "", ""

	// Define the regular expression pattern
	pattern := `\{ SectionSingle identifier: (.+?) , msgs: \[ (.+?) \] \}`

	// Compile the regular expression
	regex := regexp.MustCompile(pattern)

	// Find submatches using the regular expression
	submatches := regex.FindStringSubmatch(section)
	if submatches == nil || len(submatches) != 3 {
		return identifier, message, errors.New("invalid format of message section sequence")
	}
	identifier = submatches[1]
	message = submatches[2]
	return identifier, message, nil

}

func extractSectionSingle(data string) (string, error) {
	// Look for the prefix before the actual content
	prefix := "{ SectionSingle msg: "
	startIndex := strings.Index(data, prefix)
	if startIndex == -1 {
		return "", fmt.Errorf("start not found")
	}

	// Adjust the start index to skip the prefix
	startIndex += len(prefix)

	// We'll assume the content ends with " }" that closes the sectionSingle
	endIndex := strings.LastIndex(data[startIndex:], " }")
	if endIndex == -1 {
		return "", fmt.Errorf("end not found")
	}

	// Adjust the end index relative to the entire string
	endIndex += startIndex

	// Extract the content between the start and end indexes
	content := data[startIndex:endIndex]

	// Clean up the extracted content
	content = strings.Trim(content, " ")

	return content, nil
}

func encodeOpMsg(responseOpMsg *models.MongoOpMessage, actualRequestMsgSections []string, expectedRequestMsgSections []string, logger *zap.Logger) (*opMsg, error) {
	message := &opMsg{
		flags:    wiremessage.MsgFlag(responseOpMsg.FlagBits),
		checksum: uint32(responseOpMsg.Checksum),
		sections: []opMsgSection{},
	}
	for messageIndex, messageValue := range responseOpMsg.Sections {
		switch {
		case strings.HasPrefix(messageValue, "{ SectionSingle identifier:"):
			identifier, msgsStr, err := decodeOpMsgSectionSequence(responseOpMsg.Sections[messageIndex])
			if err != nil {
				logger.Error("failed to extract the msg section from recorded message", zap.Error(err))
				return nil, err
			}
			msgs := strings.Split(msgsStr, ", ")
			docs := []bsoncore.Document{}
			for _, msg := range msgs {
				var unmarshaledDoc bsoncore.Document
				err = bson.UnmarshalExtJSON([]byte(msg), true, &unmarshaledDoc)
				if err != nil {
					logger.Error("failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
					return nil, err
				}
				docs = append(docs, unmarshaledDoc)
			}
			message.sections = append(message.sections, &opMsgSectionSequence{
				identifier: identifier,
				msgs:       docs,
			})
		case strings.HasPrefix(messageValue, "{ SectionSingle msg:"):
			sectionStr, err := extractSectionSingle(responseOpMsg.Sections[messageIndex])
			if err != nil {
				logger.Error("failed to extract the msg section from recorded message single section", zap.Error(err))
				return nil, err
			}

			resultStr, ok, err := handleScramAuth(actualRequestMsgSections, expectedRequestMsgSections, sectionStr, logger)
			if err != nil {
				return nil, err
			}
			if ok {
				logger.Debug("new responses have been generated for the scram authentication", zap.Any("response", resultStr))
				sectionStr = resultStr
			}

			var unmarshaledDoc bsoncore.Document

			err = bson.UnmarshalExtJSON([]byte(sectionStr), true, &unmarshaledDoc)
			if err != nil {
				logger.Error("failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
				return nil, err
			}
			message.sections = append(message.sections, &opMsgSectionSingle{
				msg: unmarshaledDoc,
			})
		default:
			logger.Error("failed to encode the OpMsg section into mongo wiremessage because of invalid format", zap.Any("section", messageValue))
		}
	}
	return message, nil
}

// see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/operation.go#L1387-L1423
func decodeMsg(reqID int32, wm []byte) (*opMsg, error) {
	var ok bool
	m := opMsg{
		reqID: reqID,
	}

	m.flags, wm, ok = wiremessage.ReadMsgFlags(wm)
	if !ok {
		return nil, errors.New("malformed wire message: missing OP_MSG flags")
	}

	checksumPresent := m.flags&wiremessage.ChecksumPresent == wiremessage.ChecksumPresent
	for len(wm) > 0 {
		// If the checksumPresent flag is set, the last four bytes of the message contain the checksum.
		if checksumPresent && len(wm) == 4 {
			m.checksum, wm, ok = wiremessage.ReadMsgChecksum(wm)
			if !ok {
				return nil, errors.New("malformed wire message: insufficient bytes to read checksum")
			}
			continue
		}

		var stype wiremessage.SectionType
		stype, wm, ok = wiremessage.ReadMsgSectionType(wm)
		if !ok {
			return nil, errors.New("malformed wire message: insufficient bytes to read section type")
		}

		switch stype {
		case wiremessage.SingleDocument:
			s := opMsgSectionSingle{}
			s.msg, wm, ok = wiremessage.ReadMsgSectionSingleDocument(wm)
			if !ok {
				return nil, errors.New("malformed wire message: insufficient bytes to read single document")
			}
			m.sections = append(m.sections, &s)
		case wiremessage.DocumentSequence:
			s := opMsgSectionSequence{}
			s.identifier, s.msgs, wm, ok = wiremessage.ReadMsgSectionDocumentSequence(wm)
			if !ok {
				return nil, errors.New("malformed wire message: insufficient bytes to read document sequence")
			}
			m.sections = append(m.sections, &s)
		default:
			return nil, fmt.Errorf("malformed wire message: unknown section type %v", stype)
		}
	}

	return &m, nil
}

func (m *opMsg) OpCode() wiremessage.OpCode {
	return wiremessage.OpMsg
}

// see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/operation.go#L898-L904
func (m *opMsg) Encode(responseTo, requestId int32) []byte {
	var buffer []byte
	lOgger.Debug(fmt.Sprintf("the responseTo for the OpMsg: %v, for requestId: %v", responseTo, wiremessage.NextRequestID()))

	idx, buffer := wiremessage.AppendHeaderStart(buffer, requestId, responseTo, wiremessage.OpMsg)
	buffer = wiremessage.AppendMsgFlags(buffer, m.flags)
	for _, section := range m.sections {
		buffer = section.append(buffer)
	}
	if m.flags&wiremessage.ChecksumPresent == wiremessage.ChecksumPresent {
		// The checksum is a uint32, but we can use appendi32 to encode it. Overflow/underflow when casting to int32 is
		// not a concern here because the bytes in the number do not change after casting.
		buffer = appendi32(buffer, int32(m.checksum))
	}
	buffer = bsoncore.UpdateLength(buffer, idx, int32(len(buffer[idx:])))
	lOgger.Debug(fmt.Sprintf("opmsg string: %v", m.String()))
	return buffer
}

func (m *opMsg) IsIsMaster() bool {
	for _, section := range m.sections {
		if section.isIsMaster() {
			return true
		}
	}
	return false
}

func (m *opMsg) IsIsAdminDB() bool {
	for _, section := range m.sections {
		if section.isDbAdmin() {
			return true
		}
	}
	return false
}

func (m *opMsg) CursorID() (cursorID int64, ok bool) {
	for _, section := range m.sections {
		if cursorID, ok := section.cursorID(); ok {
			return cursorID, ok
		}
	}
	return 0, false
}

func (m *opMsg) RequestID() int32 {
	return m.reqID
}

func (m *opMsg) Error() error {
	if len(m.sections) == 0 {
		return nil
	}
	single, ok := m.sections[0].(*opMsgSectionSingle)
	if !ok {
		return nil
	}
	return driver.ExtractErrorFromServerResponse(single.msg)
}

func (m *opMsg) Unacknowledged() bool {
	return m.flags&wiremessage.MoreToCome == wiremessage.MoreToCome
}

func (m *opMsg) CommandAndCollection() (Command, string) {
	for _, section := range m.sections {
		command, collection := section.commandAndCollection()
		if command != Unknown {
			return command, collection
		}
	}
	return Unknown, ""
}

// TransactionDetails See https://github.com/mongodb/specifications/blob/master/source/transactions/transactions.rst
// Version 4.0 of the server introduces multi-statement transactions.
// opMsg is available from wire protocol 3.6
// deprecated operations such OP_UPDATE OP_INSERT are not supposed to support transaction statements.
// When constructing any other command within a transaction, drivers MUST add the lsid, txnNumber, and autocommit fields.
func (m *opMsg) TransactionDetails() *TransactionDetails {

	for _, section := range m.sections {

		if single, ok := section.(*opMsgSectionSingle); ok {
			_, lsID, ok := single.msg.Lookup("lsid", "id").BinaryOK()
			if !ok {
				continue
			}

			txnNumber, ok := single.msg.Lookup("txnNumber").Int64OK()
			if !ok {
				continue
			}

			_, ok = single.msg.Lookup("autocommit").BooleanOK()
			if !ok {
				continue
			}

			startTransaction, ok := single.msg.Lookup("startTransaction").BooleanOK()
			return &TransactionDetails{
				LsID:               lsID,
				TxnNumber:          txnNumber,
				IsStartTransaction: ok && startTransaction,
			}
		}
	}

	return nil
}

func (m *opMsg) GetFlagBits() int32 {
	return int32(m.flags)
}

func (m *opMsg) String() string {
	var sections []string
	for _, section := range m.sections {
		sections = append(sections, section.String())
	}
	return fmt.Sprintf("{ OpMsg flags: %d, sections: [%s], checksum: %d }", m.flags, strings.Join(sections, ", "), m.checksum)
}

// https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#op-reply
type opReply struct {
	reqID        int32
	flags        wiremessage.ReplyFlag
	cursorID     int64
	startingFrom int32
	numReturned  int32
	documents    []bsoncore.Document
}

func (r *opReply) TransactionDetails() *TransactionDetails {
	return nil
}

func encodeOpReply(reply *models.MongoOpReply, logger *zap.Logger) (*opReply, error) {
	replyDocs := []bsoncore.Document{}
	for _, v := range reply.Documents {
		var unmarshaledDoc bsoncore.Document
		logger.Debug(fmt.Sprintf("the document string is: %v", string(v)))
		var result map[string]interface{}

		err := json.Unmarshal([]byte(v), &result)
		if err != nil {
			logger.Error("failed to unmarshal string document of OpReply", zap.Error(err))
			return nil, err
		}
		// set the fields for handshake calls at test mode
		result["localTime"].(map[string]interface{})["$date"].(map[string]interface{})["$numberLong"] = strconv.FormatInt(time.Now().Unix(), 10)

		v, err := json.Marshal(result)
		if err != nil {
			logger.Error("failed to marshal the updated string document of OpReply", zap.Error(err))
			return nil, err
		}
		logger.Debug(fmt.Sprintf("the updated document string is: %v", result["localTime"].(map[string]interface{})["$date"].(map[string]interface{})["$numberLong"]))

		err = bson.UnmarshalExtJSON([]byte(v), false, &unmarshaledDoc)
		if err != nil {
			logger.Error("failed to decode the recorded document of OpReply", zap.Error(err))
			return nil, err
		}
		elements, _ := unmarshaledDoc.Elements()
		logger.Debug(fmt.Sprintf("the elements of the reply docs: %v", elements))
		replyDocs = append(replyDocs, unmarshaledDoc)

	}
	return &opReply{
		flags:        wiremessage.ReplyFlag(reply.ResponseFlags),
		cursorID:     reply.CursorID,
		startingFrom: reply.StartingFrom,
		numReturned:  reply.NumberReturned,
		documents:    replyDocs,
	}, nil
}

// see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/operation.go#L1297-L1358
func decodeReply(reqID int32, wm []byte) (*opReply, error) {
	var ok bool
	r := opReply{
		reqID: reqID,
	}

	r.flags, wm, ok = wiremessage.ReadReplyFlags(wm)
	if !ok {
		return nil, errors.New("malformed reply message: missing OP_REPLY flags")
	}

	r.cursorID, wm, ok = wiremessage.ReadReplyCursorID(wm)
	if !ok {
		return nil, errors.New("malformed reply message: cursor id")
	}

	r.startingFrom, wm, ok = wiremessage.ReadReplyStartingFrom(wm)
	if !ok {
		return nil, errors.New("malformed reply message: starting from")
	}

	r.numReturned, wm, ok = wiremessage.ReadReplyNumberReturned(wm)
	if !ok {
		return nil, errors.New("malformed reply message: number returned")
	}

	r.documents, _, ok = wiremessage.ReadReplyDocuments(wm)
	if !ok {
		return nil, errors.New("malformed reply message: could not read documents from reply")
	}

	return &r, nil
}

func (r *opReply) OpCode() wiremessage.OpCode {
	return wiremessage.OpReply
}

// see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/drivertest/channel_conn.go#L73-L82
func (r *opReply) Encode(responseTo, requestId int32) []byte {
	var buffer []byte
	idx, buffer := wiremessage.AppendHeaderStart(buffer, requestId, responseTo, wiremessage.OpReply)
	buffer = wiremessage.AppendReplyFlags(buffer, r.flags)
	buffer = wiremessage.AppendReplyCursorID(buffer, r.cursorID)
	buffer = wiremessage.AppendReplyStartingFrom(buffer, r.startingFrom)
	buffer = wiremessage.AppendReplyNumberReturned(buffer, r.numReturned)
	for _, doc := range r.documents {
		buffer = append(buffer, doc...)
	}
	buffer = bsoncore.UpdateLength(buffer, idx, int32(len(buffer[idx:])))
	return buffer
}

func (r *opReply) IsIsMaster() bool {
	return false
}

func (r *opReply) IsIsAdminDB() bool {
	return false
}

func (r *opReply) CursorID() (cursorID int64, ok bool) {
	return r.cursorID, true
}

func (r *opReply) RequestID() int32 {
	return r.reqID
}

func (r *opReply) Error() error {
	if len(r.documents) == 0 {
		return nil
	}
	return driver.ExtractErrorFromServerResponse(r.documents[0])
}

func (r *opReply) Unacknowledged() bool {
	return false
}

func (r *opReply) CommandAndCollection() (Command, string) {
	return Find, ""
}

func (r *opReply) String() string {
	var documents []string
	for _, document := range r.documents {
		documents = append(documents, document.String())
	}
	return fmt.Sprintf("{ OpReply flags: %d, cursorID: %d, startingFrom: %d, numReturned: %d, documents: [%s] }", r.flags, r.cursorID, r.startingFrom, r.numReturned, strings.Join(documents, ", "))
}

// https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#op-get-more
// type opGetMore struct {
// 	reqID              int32
// 	fullCollectionName string
// 	numberToReturn     int32
// 	cursorID           int64
// }

// func (g *opGetMore) TransactionDetails() *TransactionDetails {
// 	return nil
// }

// // see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/operation.go#L1297-L1358
// func decodeGetMore(reqID int32, wm []byte) (*opGetMore, error) {
// 	var ok bool
// 	g := opGetMore{
// 		reqID: reqID,
// 	}

// 	// the driver doesn't support any ReadGetMore* methods, so reuse methods from other operations

// 	_, wm, ok = wiremessage.ReadKillCursorsZero(wm)
// 	if !ok {
// 		return nil, errors.New("malformed get_more message: missing zero")
// 	}

// 	g.fullCollectionName, wm, ok = wiremessage.ReadQueryFullCollectionName(wm)
// 	if !ok {
// 		return nil, errors.New("malformed get_more message: missing full collection name")
// 	}

// 	g.numberToReturn, wm, ok = wiremessage.ReadQueryNumberToReturn(wm)
// 	if !ok {
// 		return nil, errors.New("malformed get_more message: missing number to return")
// 	}

// 	g.cursorID, _, ok = wiremessage.ReadReplyCursorID(wm)
// 	if !ok {
// 		return nil, errors.New("malformed get_more message: missing cursorID")
// 	}

// 	return &g, nil
// }

// func (g *opGetMore) OpCode() wiremessage.OpCode {
// 	return wiremessage.OpGetMore
// }

// // see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/operation_legacy.go#L284-L291
// func (g *opGetMore) Encode(responseTo, requestId int32) []byte {
// 	var buffer []byte
// 	idx, buffer := wiremessage.AppendHeaderStart(buffer, 0, responseTo, wiremessage.OpGetMore)
// 	buffer = wiremessage.AppendGetMoreZero(buffer)
// 	buffer = wiremessage.AppendGetMoreFullCollectionName(buffer, g.fullCollectionName)
// 	buffer = wiremessage.AppendGetMoreNumberToReturn(buffer, g.numberToReturn)
// 	buffer = wiremessage.AppendGetMoreCursorID(buffer, g.cursorID)
// 	buffer = bsoncore.UpdateLength(buffer, idx, int32(len(buffer[idx:])))
// 	return buffer
// }

// func (g *opGetMore) IsIsMaster() bool {
// 	return false
// }

// func (g *opGetMore) CursorID() (cursorID int64, ok bool) {
// 	return g.cursorID, true
// }

// func (g *opGetMore) RequestID() int32 {
// 	return g.reqID
// }

// func (g *opGetMore) Error() error {
// 	return nil
// }

// func (g *opGetMore) Unacknowledged() bool {
// 	return false
// }

// func (g *opGetMore) CommandAndCollection() (Command, string) {
// 	return GetMore, g.fullCollectionName
// }

// func (g *opGetMore) String() string {
// 	return fmt.Sprintf("{ OpGetMore fullCollectionName: %s, numberToReturn: %d, cursorID: %d }", g.fullCollectionName, g.numberToReturn, g.cursorID)
// }

// // https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#op_update
// type opUpdate struct {
// 	reqID              int32
// 	fullCollectionName string
// 	flags              int32
// 	selector           bsoncore.Document
// 	tools             bsoncore.Document
// }

// func (u *opUpdate) TransactionDetails() *TransactionDetails {
// 	return nil
// }

// func decodeUpdate(reqID int32, wm []byte) (*opUpdate, error) {
// 	var ok bool
// 	u := opUpdate{
// 		reqID: reqID,
// 	}

// 	u.fullCollectionName, wm, ok = readCString(wm)
// 	if !ok {
// 		return nil, errors.New("malformed tools message: full collection name")
// 	}

// 	u.flags, wm, ok = readi32(wm)
// 	if !ok {
// 		return nil, errors.New("malformed tools message: missing OP_UPDATE flags")
// 	}

// 	u.selector, wm, ok = bsoncore.ReadDocument(wm)
// 	if !ok {
// 		return nil, errors.New("malformed tools message: selector document")
// 	}

// 	u.tools, _, ok = bsoncore.ReadDocument(wm)
// 	if !ok {
// 		return nil, errors.New("malformed tools message: tools document")
// 	}

// 	return &u, nil
// }

// func (u *opUpdate) OpCode() wiremessage.OpCode {
// 	return wiremessage.OpUpdate
// }

// func (u *opUpdate) Encode(responseTo, requestId int32) []byte {
// 	var buffer []byte
// 	idx, buffer := wiremessage.AppendHeaderStart(buffer, 0, responseTo, wiremessage.OpUpdate)
// 	buffer = appendCString(buffer, u.fullCollectionName)
// 	buffer = appendi32(buffer, u.flags)
// 	buffer = append(buffer, u.selector...)
// 	buffer = append(buffer, u.tools...)
// 	buffer = bsoncore.UpdateLength(buffer, idx, int32(len(buffer[idx:])))
// 	return buffer
// }

// func (u *opUpdate) IsIsMaster() bool {
// 	return false
// }

// func (u *opUpdate) CursorID() (cursorID int64, ok bool) {
// 	return 0, false
// }

// func (u *opUpdate) RequestID() int32 {
// 	return u.reqID
// }

// func (u *opUpdate) Error() error {
// 	return nil
// }

// func (u *opUpdate) Unacknowledged() bool {
// 	return false
// }

// func (u *opUpdate) CommandAndCollection() (Command, string) {
// 	return Update, u.fullCollectionName
// }

// func (u *opUpdate) String() string {
// 	return fmt.Sprintf("{ OpQuery fullCollectionName: %s, flags: %d, selector: %s, tools: %s }", u.fullCollectionName, u.flags, u.selector.String(), u.tools.String())
// }

// // https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#op_insert
// type opInsert struct {
// 	reqID              int32
// 	flags              int32
// 	fullCollectionName string
// 	documents          []bsoncore.Document
// }

// func (i *opInsert) TransactionDetails() *TransactionDetails {
// 	return nil
// }

// func decodeInsert(reqID int32, wm []byte) (*opInsert, error) {
// 	var ok bool
// 	i := opInsert{
// 		reqID: reqID,
// 	}

// 	i.flags, wm, ok = readi32(wm)
// 	if !ok {
// 		return nil, errors.New("malformed insert message: missing OP_INSERT flags")
// 	}

// 	i.fullCollectionName, wm, ok = readCString(wm)
// 	if !ok {
// 		return nil, errors.New("malformed insert message: full collection name")
// 	}

// 	i.documents, _, ok = wiremessage.ReadReplyDocuments(wm)
// 	if !ok {
// 		return nil, errors.New("malformed insert message: could not read documents")
// 	}

// 	return &i, nil
// }

// func (i *opInsert) OpCode() wiremessage.OpCode {
// 	return wiremessage.OpInsert
// }

// func (i *opInsert) Encode(responseTo, requestId int32) []byte {
// 	var buffer []byte
// 	idx, buffer := wiremessage.AppendHeaderStart(buffer, 0, responseTo, wiremessage.OpInsert)
// 	buffer = appendi32(buffer, i.flags)
// 	buffer = appendCString(buffer, i.fullCollectionName)
// 	for _, doc := range i.documents {
// 		buffer = append(buffer, doc...)
// 	}
// 	buffer = bsoncore.UpdateLength(buffer, idx, int32(len(buffer[idx:])))
// 	return buffer
// }

// func (i *opInsert) IsIsMaster() bool {
// 	return false
// }

// func (i *opInsert) CursorID() (cursorID int64, ok bool) {
// 	return 0, false
// }

// func (i *opInsert) RequestID() int32 {
// 	return i.reqID
// }

// func (i *opInsert) Error() error {
// 	return nil
// }

// func (i *opInsert) Unacknowledged() bool {
// 	return false
// }

// func (i *opInsert) CommandAndCollection() (Command, string) {
// 	return Insert, i.fullCollectionName
// }

// func (i *opInsert) String() string {
// 	var documents []string
// 	for _, document := range i.documents {
// 		documents = append(documents, document.String())
// 	}
// 	return fmt.Sprintf("{ OpInsert flags: %d, fullCollectionName: %s, documents: %s }", i.flags, i.fullCollectionName, strings.Join(documents, ", "))
// }

// // https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#op_insert
// type opDelete struct {
// 	reqID              int32
// 	fullCollectionName string
// 	flags              int32
// 	selector           bsoncore.Document
// }

// func (d *opDelete) TransactionDetails() *TransactionDetails {
// 	return nil
// }

// func decodeDelete(reqID int32, wm []byte) (*opDelete, error) {
// 	var ok bool
// 	d := opDelete{
// 		reqID: reqID,
// 	}

// 	_, wm, ok = readi32(wm)
// 	if !ok {
// 		return nil, errors.New("malformed delete message: missing zero")
// 	}

// 	d.fullCollectionName, wm, ok = readCString(wm)
// 	if !ok {
// 		return nil, errors.New("malformed delete message: full collection name")
// 	}

// 	d.flags, wm, ok = readi32(wm)
// 	if !ok {
// 		return nil, errors.New("malformed delete message: missing OP_DELETE flags")
// 	}

// 	d.selector, _, ok = bsoncore.ReadDocument(wm)
// 	if !ok {
// 		return nil, errors.New("malformed delete message: selector document")
// 	}

// 	return &d, nil
// }

// func (d *opDelete) OpCode() wiremessage.OpCode {
// 	return wiremessage.OpDelete
// }

// func (d *opDelete) Encode(responseTo, requestId int32) []byte {
// 	var buffer []byte
// 	idx, buffer := wiremessage.AppendHeaderStart(buffer, 0, responseTo, wiremessage.OpDelete)
// 	buffer = appendCString(buffer, d.fullCollectionName)
// 	buffer = appendi32(buffer, d.flags)
// 	buffer = append(buffer, d.selector...)
// 	buffer = bsoncore.UpdateLength(buffer, idx, int32(len(buffer[idx:])))
// 	return buffer
// }

// func (d *opDelete) IsIsMaster() bool {
// 	return false
// }

// func (d *opDelete) CursorID() (cursorID int64, ok bool) {
// 	return 0, false
// }

// func (d *opDelete) RequestID() int32 {
// 	return d.reqID
// }

// func (d *opDelete) Error() error {
// 	return nil
// }

// func (d *opDelete) Unacknowledged() bool {
// 	return false
// }

// func (d *opDelete) CommandAndCollection() (Command, string) {
// 	return Delete, d.fullCollectionName
// }

// func (d *opDelete) String() string {
// 	return fmt.Sprintf("{ OpDelete fullCollectionName: %s, flags: %d, selector: %s }", d.fullCollectionName, d.flags, d.selector.String())
// }

// // https://docs.mongodb.com/manual/reference/mongodb-wire-protocol/#op_kill_cursors
// type opKillCursors struct {
// 	reqID     int32
// 	cursorIDs []int64
// }

// func (k *opKillCursors) TransactionDetails() *TransactionDetails {
// 	return nil
// }

// func decodeKillCursors(reqID int32, wm []byte) (*opKillCursors, error) {
// 	var ok bool
// 	k := opKillCursors{
// 		reqID: reqID,
// 	}

// 	_, wm, ok = wiremessage.ReadKillCursorsZero(wm)
// 	if !ok {
// 		return nil, errors.New("malformed kill_cursors message: missing zero")
// 	}

// 	var numIDs int32
// 	numIDs, wm, ok = wiremessage.ReadKillCursorsNumberIDs(wm)
// 	if !ok {
// 		return nil, errors.New("malformed kill_cursors message: missing number of cursor IDs")
// 	}

// 	k.cursorIDs, _, ok = wiremessage.ReadKillCursorsCursorIDs(wm, numIDs)
// 	if !ok {
// 		return nil, errors.New("malformed kill_cursors message: missing cursor IDs")
// 	}

// 	return &k, nil
// }

// func (k *opKillCursors) OpCode() wiremessage.OpCode {
// 	return wiremessage.OpKillCursors
// }

// // see https://github.com/mongodb/mongo-go-driver/blob/v1.7.2/x/mongo/driver/operation_legacy.go#L378-L384
// func (k *opKillCursors) Encode(responseTo, requestId int32) []byte {
// 	var buffer []byte
// 	idx, buffer := wiremessage.AppendHeaderStart(buffer, 0, responseTo, wiremessage.OpKillCursors)
// 	buffer = wiremessage.AppendKillCursorsZero(buffer)
// 	buffer = wiremessage.AppendKillCursorsNumberIDs(buffer, int32(len(k.cursorIDs)))
// 	buffer = wiremessage.AppendKillCursorsCursorIDs(buffer, k.cursorIDs)
// 	buffer = bsoncore.UpdateLength(buffer, idx, int32(len(buffer[idx:])))
// 	return buffer
// }

// func (k *opKillCursors) IsIsMaster() bool {
// 	return false
// }

// func (k *opKillCursors) CursorID() (cursorID int64, ok bool) {
// 	return 0, false
// }

// func (k *opKillCursors) RequestID() int32 {
// 	return k.reqID
// }

// func (k *opKillCursors) Error() error {
// 	return nil
// }

// func (k *opKillCursors) Unacknowledged() bool {
// 	return false
// }

// func (k *opKillCursors) CommandAndCollection() (Command, string) {
// 	return Unknown, ""
// }

// func (k *opKillCursors) String() string {
// 	return fmt.Sprintf("{ OpKillCursors cursorIDs: %v }", k.cursorIDs)
// }

func appendi32(dst []byte, i32 int32) []byte {
	return append(dst, byte(i32), byte(i32>>8), byte(i32>>16), byte(i32>>24))
}

func appendCString(b []byte, str string) []byte {
	b = append(b, str...)
	return append(b, 0x00)
}

// func readi32(src []byte) (int32, []byte, bool) {
// 	if len(src) < 4 {
// 		return 0, src, false
// 	}

// 	return int32(src[0]) | int32(src[1])<<8 | int32(src[2])<<16 | int32(src[3])<<24, src[4:], true
// }

// func readCString(src []byte) (string, []byte, bool) {
// 	idx := bytes.IndexByte(src, 0x00)
// 	if idx < 0 {
// 		return "", src, false
// 	}
// 	return string(src[:idx]), src[idx+1:], true
// }
