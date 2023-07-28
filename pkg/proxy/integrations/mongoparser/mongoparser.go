package mongoparser

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"reflect"
	"strings"
	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

var Emoji = "\U0001F430" + " Keploy:"
var configRequests = []string{""}

// IsOutgoingMongo function determines if the outgoing network call is Mongo by comparing the
// message format with that of a mongo wire message.
func IsOutgoingMongo(buffer []byte) bool {
	if len(buffer) < 4 {
		return false
	}
	messageLength := binary.LittleEndian.Uint32(buffer[0:4])
	return int(messageLength) == len(buffer)
}

func ProcessOutgoingMongo(clientConnId, destConnId int, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger) {
	// fmt.Println("into processing mongo. clientConnId: ", clientConnId)
	switch models.GetMode() {
	case models.MODE_RECORD:
		// capturedDeps := encodeOutgoingMongo(requestBuffer, clientConn, destConn, logger)
		encodeOutgoingMongo(clientConnId, destConnId, requestBuffer, clientConn, destConn, h, started, readRequestDelay, logger)

		// *deps = append(*deps, capturedDeps...)
		// for _, v := range capturedDeps {
		// 	h.AppendDeps(v)
		// 	// h.WriteMock(v)
		// }
	case models.MODE_TEST:
		// fmt.Println("into test mode. clientConnId: ", clientConnId)
		decodeOutgoingMongo(clientConnId, destConnId, requestBuffer, clientConn, destConn, h, started, readRequestDelay, logger)
	default:
	}
}

// func decodeOutgoingMongo(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
// 	// var helloReply, replyMsg = []byte{}, []byte{}
// 	// _, _, _, _, _, err := Decode(buffer)
// 	fmt.Println("into decode mongo.")
// 	_, requestHeader, _, err := Decode(requestBuffer)
// 	if err != nil {
// 		logger.Error(Emoji+"failed to decode the mongo request wiremessage", zap.Error(err))
// 		return
// 		// log.Println("failed to decode the mongo wire message", err)
// 		// break
// 	}
// 	// if len(deps) <= 1 {
// 	if h.GetDepsSize() <= 1 {
// 		// logger.Error("failed to mock the output for unrecorded outgoing mongo query")
// 		// log.Println("the dep call is not mocked during record")
// 		return
// 		// } else {
// 		// 	helloReply = deps[0].Spec.Objects[1].Data
// 		// 	replyMsg = deps[1].Spec.Objects[1].Data
// 	}
// 	// fmt.Println("deps: ", *deps[0], "\n", *deps[1], "\n\n ")
// 	// mongoSpec := deps[0]
// 	mongoSpec := h.FetchDep(0)
// 	// var mongoSpec spec.MongoSpec
// 	// err = deps[0].Spec.Decode(&mongoSpec)
// 	// if err != nil {
// 	// 	logger.Error("failed to decode the yaml spec for the outgoing mongo call")
// 	// 	return
// 	// }
// 	// fmt.Printf("mongoSpec: %v\n", mongoSpec)
// 	// querySpec, ok := mongoSpec.Spec.MongoRequest.(models.MongoOpQuery)
// 	// if !ok {
// 	// 	logger.Error("failed to decode the yaml spec for the outgoing mongo request")
// 	// 	return
// 	// }
// 	// var querySpec spec.MongoOpQuery
// 	// err = mongoSpec.Request.Decode(&querySpec)
// 	// if err != nil {
// 	// 	logger.Error("failed to decode the yaml spec for the outgoing mongo request", ap.Error(err))
// 	// 	return
// 	// }
// 	replySpec, ok := mongoSpec.Spec.MongoResponse.(*models.MongoOpReply)
// 	if !ok {
// 		logger.Error(Emoji + "failed to decode the yaml for mongo OpReply wiremessage")
// 		return
// 	}
// 	// var replySpec spec.MongoOpReply = spec.MongoOpReply{}
// 	// err = mongoSpec.Response.Decode(&replySpec)
// 	// if err != nil {
// 	// 	logger.Error("failed to decode the yaml spec for the outgoing mongo reply")
// 	// 	return
// 	// }
// 	// println("the replyspec is: ", replySpec.ResponseFlags)
// 	// opReply
// 	replyDocs := []bsoncore.Document{}
// 	for _, v := range replySpec.Documents {
// 		var unmarshaledDoc bsoncore.Document
// 		err = bson.UnmarshalExtJSON([]byte(v), false, &unmarshaledDoc)
// 		if err != nil {
// 			logger.Error(Emoji+"failed to unmarshal the recorded document string of OpReply", zap.Error(err))
// 			return
// 		}
// 		// docs, rm, ok := bsoncore.ReadDocument([]byte(v))
// 		// fmt.Println("the document in healtcheck of test mode: ", docs.String(), " rm bytes: ", rm,  " ok: ", ok)
// 		// replyDocs = append(replyDocs, bsoncore.Document(v))
// 		replyDocs = append(replyDocs, unmarshaledDoc)
// 		// fmt.Println("the documents in healtcheck of test mode: ", replyDocs)
// 	}
// 	// fmt.Println("the documents in healtcheck of test mode: ", replyDocs)
// 	// reply := &opReply{
// 	// 	flags: wiremessage.ReplyFlag(replySpec.ResponseFlags),
// 	// 	cursorID: replySpec.CursorID,
// 	// 	startingFrom: replySpec.StartingFrom,
// 	// 	numReturned: replySpec.NumberReturned,
// 	// 	documents: replyDocs,
// 	// 	reqID: requestHeader.RequestID,
// 	// }
// 	// println("string format for the created opcode msg: ", reply.String())
// 	var heathCheckReplyBuffer []byte
// 	heathCheckReplyBuffer = wiremessage.AppendHeader(heathCheckReplyBuffer, mongoSpec.Spec.MongoResponseHeader.Length, requestHeader.RequestID+1, requestHeader.RequestID, mongoSpec.Spec.MongoResponseHeader.Opcode)
// 	heathCheckReplyBuffer = wiremessage.AppendReplyFlags(heathCheckReplyBuffer, wiremessage.ReplyFlag(replySpec.ResponseFlags))
// 	heathCheckReplyBuffer = wiremessage.AppendReplyCursorID(heathCheckReplyBuffer, replySpec.CursorID)
// 	heathCheckReplyBuffer = wiremessage.AppendReplyStartingFrom(heathCheckReplyBuffer, replySpec.StartingFrom)
// 	heathCheckReplyBuffer = wiremessage.AppendReplyNumberReturned(heathCheckReplyBuffer, replySpec.NumberReturned)
// 	for _, doc := range replyDocs {
// 		heathCheckReplyBuffer = append(heathCheckReplyBuffer, doc...)
// 	}
// 	// heathCheckReplyBuffer = append(heathCheckReplyBuffer, reply.Encode(requestHeader.ResponseTo)...)
// 	// opr, _, _, err = Decode(heathCheckReplyBuffer)
// 	// println("the healthcheck response : ", opr.String())
// 	_, err = clientConn.Write(heathCheckReplyBuffer)
// 	if err != nil {
// 		logger.Error(Emoji+"failed to write the health check reply to mongo client", zap.Error(err))
// 		return
// 		// log.Printf("failed to write response to the client conn. error: %v \n", err.Error())
// 	}
// 	// _, _, _, _, _, err = Decode(helloReply)
// 	// if err != nil {
// 	// 	log.Println("failed to decode the mongo wire message", err)
// 	// 	// break
// 	// }
// 	// if !strings.Contains(op.String(), "hello") {
// 	// 	log.Printf("the decoded response wire message: length: %v, reqId: %v, respTo: %v, opCode: %v, body: %v on mode: %v", l, reqId, respTo, opCode, op.String(), os.Getenv("KEPLOY_MODE"))
// 	// }
// 	operationBuffer, err := util.ReadBytes(clientConn)
// 	if err != nil {
// 		logger.Error(Emoji+"failed to read the mongo wiremessage for operation query", zap.Error(err))
// 		return
// 		// log.Printf("failed to read the message. error: %v\n", err)
// 		// break
// 	}
// 	fmt.Println("after hertbeat interchange in decode.")
// 	opr1, _, _, err := Decode(operationBuffer)
// 	if err != nil {
// 		logger.Error(Emoji+"failed to decode the mongo operation query", zap.Error(err))
// 		// panic("stop due to invalid mongo wiremessage")
// 		return
// 		// log.Printf("failed to decode the mongo wire message. error: %v", err.Error())
// 		// break
// 	}
// 	// logger.Error(Emoji + "the operation buffer: ", zap.String("", opr1.String()))
// 	// panic("stop recuring loop")
// 	if strings.Contains(opr1.String(), "hello") {
// 		return
// 	}
// 	// if !strings.Contains(opr1.String(), "hello") {
// 	// 	log.Printf("the decoded wire message: length: %v, reqId: %v, respTo: %v, opCode: %v, body: %v on mode: %v", lr, reqIdr, respTor, opCoder, opr1.String(), os.Getenv("KEPLOY_MODE"))
// 	// }
// 	// log.Println("length of deps: ", len(deps))
// 	// log.Println(", length of objects: ", len(deps[1].Spec.Objects))
// 	// mongoSpec1 := deps[1]
// 	mongoSpec1 := h.FetchDep(1)
// 	// 	var mongoSpec1 spec.MongoSpec
// 	// err = deps[1].Spec.Decode(&mongoSpec1)
// 	// if err != nil {
// 	// 	logger.Error("failed to decode the yaml spec for the outgoing mongo call")
// 	// 	return
// 	// }
// 	// msgQuerySpec, ok := mongoSpec1.Spec.MongoRequest.(models.MongoOpMessage)
// 	// if !ok {
// 	// 	logger.Error("failed to decode the yaml for mongo OpMessage wiremessage request")
// 	// 	return
// 	// }
// 	// var msgQuerySpec spec.MongoOpMessage
// 	// err = mongoSpec1.Request.Decode(&msgQuerySpec)
// 	// if err != nil {
// 	// 	logger.Error("failed to decode the yaml spec for the outgoing mongo request")
// 	// 	return
// 	// }
// 	msgSpec, ok := mongoSpec1.Spec.MongoResponse.(*models.MongoOpMessage)
// 	// fmt.Println("mock for the opmsg: ", mongoSpec1.Spec.MongoResponse, "\n ")
// 	if !ok {
// 		logger.Error(Emoji + "failed to decode the yaml for mongo OpMessage wiremessage response")
// 		return
// 	}
// 	// var msgSpec spec.MongoOpMessage
// 	// err = mongoSpec1.Response.Decode(&msgSpec)
// 	// if err != nil {
// 	// 	logger.Error("failed to decode the yaml spec for the outgoing mongo reply")
// 	// 	return
// 	// }
// 	// fmt.Println("the msg spec: ", msgSpec)
// 	// wiremessage.
// 	msg := &opMsg{
// 		flags:    wiremessage.MsgFlag(msgSpec.FlagBits),
// 		checksum: uint32(msgSpec.Checksum),
// 		sections: []opMsgSection{},
// 	}
// 	// if len(msgSpec.Sections) == 1 {
// 	// 	// sectionStr := strings.Trim(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3], " ")
// 	// 	// sectionStr := msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-2]
// 	// 	var sectionStr string
// 	// 	_, err := fmt.Sscanf(msgSpec.Sections[0], "{ SectionSingle msg: %s }", &sectionStr)
// 	// 	if err != nil {
// 	// 		logger.Error("failed to scan the message section from the recorded section string", zap.Error(err))
// 	// 		return
// 	// 	}
// 	// 	// logger.Info("the section in the msg response", zap.Any("", sectionStr))
// 	// 	var unmarshaledDoc bsoncore.Document
// 	// 	// err = bson.UnmarshalExtJSON([]byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3]), false, &unmarshaledDoc)
// 	// 	err = bson.UnmarshalExtJSON([]byte(sectionStr), false, &unmarshaledDoc)
// 	// 	if err != nil {
// 	// 		logger.Error("failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
// 	// 		return
// 	// 	}
// 	// 	// fmt.Println("the unmarshaled doc: ", unmarshaledDoc)
// 	// 	// msg.sections = []opMsgSection{&opMsgSectionSingle{msg: []byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3])}}
// 	// 	msg.sections = []opMsgSection{&opMsgSectionSingle{
// 	// 		msg: unmarshaledDoc,
// 	// 	}}
// 	// } else {
// 	for i, v := range msgSpec.Sections {
// 		if strings.Contains(v, "SectionSingle identifier") {
// 			// sectionStr := strings.Trim(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3], " ")
// 			// sectionStr := msgSpec.Sections[i][21 : len(msgSpec.Sections[i])-2]
// 			// logger.Info("the section in the msg response", zap.Any("", sectionStr))
// 			var identifier string
// 			var msgsStr string
// 			_, err := fmt.Sscanf(msgSpec.Sections[i], "{ SectionSingle identifier: %s, msgs: [%s] }", &identifier, &msgsStr)
// 			if err != nil {
// 				logger.Error(Emoji+"failed to extract the msg section from recorded message", zap.Error(err))
// 				return
// 			}
// 			msgs := strings.Split(msgsStr, ", ")
// 			docs := []bsoncore.Document{}
// 			for _, v := range msgs {
// 				var unmarshaledDoc bsoncore.Document
// 				// err = bson.UnmarshalExtJSON([]byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3]), false, &unmarshaledDoc)
// 				err = bson.UnmarshalExtJSON([]byte(v), false, &unmarshaledDoc)
// 				if err != nil {
// 					logger.Error(Emoji+"failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
// 					return
// 				}
// 				docs = append(docs, unmarshaledDoc)
// 			}
// 			// msg.sections = []opMsgSection{&opMsgSectionSingle{msg: []byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3])}}
// 			// msg.sections = []opMsgSection{&opMsgSectionSingle{
// 			// 	msg: unmarshaledDoc,
// 			// }}
// 			msg.sections = append(msg.sections, &opMsgSectionSequence{
// 				// msg: unmarshaledDoc,
// 				identifier: identifier,
// 				msgs:       docs,
// 			})
// 		} else {
// 			// sectionStr := strings.Trim(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3], " ")
// 			// sectionStr := msgSpec.Sections[i][21 : len(msgSpec.Sections[i])-2]
// 			var sectionStr string
// 			_, err := fmt.Sscanf(msgSpec.Sections[0], "{ SectionSingle msg: %s }", &sectionStr)
// 			if err != nil {
// 				logger.Error(Emoji+"failed to extract the msg section from recorded message single section", zap.Error(err))
// 				return
// 			}
// 			// logger.Info("the section in the msg response", zap.Any("", sectionStr))
// 			var unmarshaledDoc bsoncore.Document
// 			// err = bson.UnmarshalExtJSON([]byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3]), false, &unmarshaledDoc)
// 			err = bson.UnmarshalExtJSON([]byte(sectionStr), false, &unmarshaledDoc)
// 			if err != nil {
// 				logger.Error(Emoji+"failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
// 				return
// 			}
// 			// msg.sections = []opMsgSection{&opMsgSectionSingle{msg: []byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3])}}
// 			// msg.sections = []opMsgSection{&opMsgSectionSingle{
// 			// 	msg: unmarshaledDoc,
// 			// }}
// 			msg.sections = append(msg.sections, &opMsgSectionSingle{
// 				msg: unmarshaledDoc,
// 			})
// 		}
// 	}
// 	// }
// 	_, err = clientConn.Write(msg.Encode(mongoSpec1.Spec.MongoRequestHeader.ResponseTo))
// 	if err != nil {
// 		logger.Error(Emoji+"failed to write the OpMsg response for the mongo operation", zap.Error(err))
// 		return
// 		// log.Printf("failed to write response to the client conn. error: %v \n", err.Error())
// 	}
// 	// _, _, _, _, _, err = Decode(replyMsg)
// 	// if err != nil {
// 	// 	log.Println("failed to decode the mongo wire message", err)
// 	// 	// break
// 	// }
// 	// if !strings.Contains(op.String(), "hello") {
// 	// 	log.Printf("the decoded response wire message: length: %v, reqId: %v, respTo: %v, opCode: %v, body: %v on mode: %v", l, reqId, respTo, opCode, op.String(), os.Getenv("KEPLOY_MODE"))
// 	// }
// 	// deps = deps[2:]
// 	h.PopFront()
// 	h.PopFront()
// }

func decodeOutgoingMongo(clientConnId, destConnId int, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger) {
	// fmt.Println("into decode mongo. clientConnId: ", clientConnId)
	startedDecoding := time.Now()
	for {
		configMocks := h.GetConfigMocks()
		tcsMocks := h.GetTcsMocks()
		var (
			mongoRequests = []models.MongoRequest{}
			// mongoResponses = []models.MongoResponse{}
			err error
		)
		if string(requestBuffer) == "read form client connection" {
			// lstr := ""
			started := time.Now()
			requestBuffer, err = util.ReadBytes(clientConn)
			if err != nil {
				logger.Error(Emoji+"failed to read request from the mongo client", zap.Error(err), zap.Any("clientConnId", clientConnId))
				return
			}
			readRequestDelay = time.Since(started)
			// fmt.Println(lstr, " the clientConnId: ", clientConnId)
			// logStr += lstr
		}
		if len(requestBuffer) == 0 {
			return
		}
		logger.Debug(fmt.Sprintf("the lopp starts for clientConnId: %v and the time delay: %v", clientConnId, time.Since(startedDecoding)))
		opReq, requestHeader, mongoRequest, err := Decode((requestBuffer), logger)
		if err != nil {
			logger.Error(Emoji+"failed to decode the mongo wire message from the client", zap.Error(err), zap.Any("clientConnId", clientConnId))
			return
		}
		mongoRequests = append(mongoRequests, models.MongoRequest{
			Header:    &requestHeader,
			Message:   mongoRequest,
			ReadDelay: int64(readRequestDelay),
		})
		// fmt.Println("the request buffer: ", string(requestBuffer), " clientConnId: ", clientConnId)
		if val, ok := mongoRequest.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
			for {
				// fmt.Println("into the more_to_come", logStr)
				// tmpStr := ""
				started = time.Now()
				requestBuffer1, err := util.ReadBytes(clientConn)
				// logStr += tmpStr
				if err != nil {
					logger.Error(Emoji+"failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
					return
				}
				// fmt.Println(tmpStr, " the clientConnId: ", clientConnId)
				readRequestDelay = time.Since(started)

				if len(requestBuffer1) == 0 {
					logger.Debug("the response from the server is complete")
					break
				}
				// logStr += fmt.Sprintln("the response from the mongo server: ", string(responseBuffer1))
				// fmt.Println("the response from the mongo server: ", string(requestBuffer1))
				_, reqHeader, mongoReq, err := Decode(requestBuffer1, logger)
				if err != nil {
					logger.Error(Emoji+"failed to decode the mongo wire message from the mongo client", zap.Error(err), zap.Any("clientConnId", clientConnId))
					return
				}
				if mongoReqVal, ok := mongoReq.(models.MongoOpMessage); ok && !hasSecondSetBit(mongoReqVal.FlagBits) {
					logger.Debug("the request from the client is complete since the more_to_come flagbit is 0")
					break
				}
				// fmt.Println("the docoded responseHeader: ", reqHeader, " || and the response: ", mongoReq.(*models.MongoOpMessage))
				mongoRequests = append(mongoRequests, models.MongoRequest{
					Header:    &reqHeader,
					Message:   mongoReq,
					ReadDelay: int64(readRequestDelay),
				})
			}
		}
		if isHeartBeat(opReq, *mongoRequests[0].Header, mongoRequests[0].Message) {
			// fmt.Println("recieved a heartbeat call. Recieved requests: ", fmt.Sprintf("len: %v", len(mongoRequests)), " clientconnid: ", clientConnId)
			configResponseWritten := false
			for _, configMock := range configMocks {
				if len(configMock.Spec.MongoRequests) == len(mongoRequests) {
					for i, req := range configMock.Spec.MongoRequests {
						// fmt.Println()
						// fmt.Println("the recieved request: ", fmt.Sprintf(" header: %v", mongoRequests[i].Header), " \nthe expected request: ", fmt.Sprintf(" header: %v", req.Header))
						if req.Header.Length != mongoRequests[i].Header.Length || req.Header.Opcode != mongoRequests[i].Header.Opcode {
							continue
						}
						switch req.Header.Opcode {
						case wiremessage.OpQuery:
							expectedQuery := req.Message.(*models.MongoOpQuery)
							actualQuery := mongoRequests[i].Message.(*models.MongoOpQuery)
							// fmt.Println("the actual request query :", fmt.Sprintf("%v", actualQuery), "\n expected query: ", fmt.Sprintf("%v", expectedQuery))
							if actualQuery.FullCollectionName != expectedQuery.FullCollectionName ||
								actualQuery.ReturnFieldsSelector != expectedQuery.ReturnFieldsSelector ||
								actualQuery.Flags != expectedQuery.Flags ||
								actualQuery.NumberToReturn != expectedQuery.NumberToReturn ||
								actualQuery.NumberToSkip != expectedQuery.NumberToSkip ||
								actualQuery.Query != expectedQuery.Query {
								continue
							}
							// fmt.Println("the request query matches. clientconnid: ", clientConnId)

							replySpec := configMock.Spec.MongoResponses[0].Message.(*models.MongoOpReply)
							replyMessage, err := encodeOpReply(replySpec, logger)
							if err != nil {
								logger.Error(fmt.Sprintf("failed to encode the recorded OpReply yaml"), zap.Error(err), zap.Any("for request with id", mongoRequests[i].Header.RequestID))
								return
							}
							// replyDocs := []bsoncore.Document{}
							// for _, v := range replySpec.Documents {
							// 	var unmarshaledDoc bsoncore.Document
							// 	logger.Debug(fmt.Sprintf("the document string is: %v", string(v)))
							// 	var result map[string]interface{}
							// 	err := json.Unmarshal([]byte(v), &result)
							// 	if err != nil {
							// 		logger.Error("failed to unmarshal string document of OpReply", zap.Error(err))
							// 		return
							// 	}
							// 	result["localTime"].(map[string]interface{})["$date"].(map[string]interface{})["$numberLong"] = strconv.FormatInt(time.Now().Unix(), 10)
							// 	v, err := json.Marshal(result)
							// 	if err != nil {
							// 		logger.Error("failed to marshal the updated string document of OpReply", zap.Error(err))
							// 		return
							// 	}
							// 	logger.Debug(fmt.Sprintf("the updated document string is: %v", result["localTime"].(map[string]interface{})["$date"].(map[string]interface{})["$numberLong"]))
							// 	err = bson.UnmarshalExtJSON([]byte(v), false, &unmarshaledDoc)
							// 	if err != nil {
							// 		logger.Error("failed to decode the recorded document of OpReply", zap.Error(err))
							// 		return
							// 	}
							// 	// unmarshaledDoc.Lookup("localTime")
							// 	// unmarshaledDoc = bso{}
							// 	// unmarshaledDoc.
							// 	elements, _ := unmarshaledDoc.Elements()
							// 	// updatedBsonDoc := bsoncore.Document{}
							// 	// unmarshaledDoc.look
							// 	// for _, el := range elements {
							// 	// 	if el.Key() == "localTime" {
							// 	// 		logger.Debug( fmt.Sprintf("the values of localtime: %v", el.Value()) )
							// 	// 		continue
							// 	// 	}
							// 	// 	el.
							// 	// 	// updatedBsonDoc = append(updatedBsonDoc, bsoncore.BuildDocumentElement()...)
							// 	// }
							// 	logger.Debug(fmt.Sprintf("the elements of the reply docs: %v", elements))
							// 	// docs, rm, ok := bsoncore.ReadDocument([]byte(v))
							// 	// fmt.Println("the document in healtcheck of test mode: ", docs.String(), " rm bytes: ", rm,  " ok: ", ok)
							// 	// replyDocs = append(replyDocs, bsoncore.Document(v))
							// 	replyDocs = append(replyDocs, unmarshaledDoc)
							// 	// fmt.Println("the documents in healtcheck of test mode: ", replyDocs)
							// }
							// fmt.Println("the documents in healtcheck of test mode: ", replyDocs)
							// reply := &opReply{
							// 	flags: wiremessage.ReplyFlag(replySpec.ResponseFlags),
							// 	cursorID: replySpec.CursorID,
							// 	startingFrom: replySpec.StartingFrom,
							// 	numReturned: replySpec.NumberReturned,
							// 	documents: replyDocs,
							// 	reqID: requestHeader.RequestID,
							// }
							// println("string format for the created opcode msg: ", reply.String())

							heathCheckReplyBuffer := replyMessage.Encode(mongoRequests[i].Header.RequestID, wiremessage.NextRequestID())
							// var heathCheckReplyBuffer []byte
							// fmt.Printf("the index of recived request: %v. And the request header: %v clientConnId: %v\n\n", i, mongoRequests[i].Header, clientConnId)
							// heathCheckReplyBuffer = wiremessage.AppendHeader(heathCheckReplyBuffer, configMock.Spec.MongoResponses[0].Header.Length, 0, mongoRequests[i].Header.RequestID, configMock.Spec.MongoResponses[0].Header.Opcode)
							// heathCheckReplyBuffer = wiremessage.AppendReplyFlags(heathCheckReplyBuffer, wiremessage.ReplyFlag(replySpec.ResponseFlags))
							// heathCheckReplyBuffer = wiremessage.AppendReplyCursorID(heathCheckReplyBuffer, replySpec.CursorID)
							// heathCheckReplyBuffer = wiremessage.AppendReplyStartingFrom(heathCheckReplyBuffer, replySpec.StartingFrom)
							// heathCheckReplyBuffer = wiremessage.AppendReplyNumberReturned(heathCheckReplyBuffer, replySpec.NumberReturned)
							// for _, doc := range replyDocs {
							// 	heathCheckReplyBuffer = append(heathCheckReplyBuffer, doc...)
							// }
							// heathCheckReplyBuffer = append(heathCheckReplyBuffer, reply.Encode(requestHeader.ResponseTo)...)
							// opr, _, _, err = Decode(heathCheckReplyBuffer)
							// println("the healthcheck response : ", opr.String())

							logger.Debug(fmt.Sprintf("the bufffer response is: %v", string(heathCheckReplyBuffer)), zap.Any("clientconnid", clientConnId))
							_, err = clientConn.Write(heathCheckReplyBuffer)
							if err != nil {
								logger.Error("failed to write the health check reply to mongo client", zap.Error(err))
								return
								// log.Printf("failed to write response to the client conn. error: %v \n", err.Error())
							}
							configResponseWritten = true

						case wiremessage.OpMsg:
							if req.Message.(*models.MongoOpMessage).FlagBits != mongoRequests[i].Message.(*models.MongoOpMessage).FlagBits {
								continue
							}
							isMatched := true
							for sectionIndx, section := range req.Message.(*models.MongoOpMessage).Sections {
								// if section != mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx] {
								if compareOpMsgSection(section, mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx], logger) == 0.0 {
									isMatched = false
									break
								}
							}
							if isMatched {
								for _, resp := range configMock.Spec.MongoResponses {
									respMessage := resp.Message.(*models.MongoOpMessage)

									message, err := encodeOpMsg(respMessage, logger)
									if err != nil {
										logger.Error("failed to encode the recorded OpMsg response", zap.Error(err), zap.Any("for request with id", mongoRequests[i].Header.RequestID))
										return
									}

									// message := &opMsg{
									// 	flags:    wiremessage.MsgFlag(respMessage.FlagBits),
									// 	checksum: uint32(respMessage.Checksum),
									// 	sections: []opMsgSection{},
									// }
									// for messageIndex, messageValue := range respMessage.Sections {
									// 	if strings.Contains(messageValue, "SectionSingle identifier") {
									// 		// sectionStr := strings.Trim(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3], " ")
									// 		// sectionStr := msgSpec.Sections[i][21 : len(msgSpec.Sections[i])-2]
									// 		// logger.Info("the section in the msg response", zap.Any("", sectionStr))
									// 		var identifier string
									// 		var msgsStr string
									// 		_, err := fmt.Sscanf(respMessage.Sections[messageIndex], "{ SectionSingle identifier: %s, msgs: [%s] }", &identifier, &msgsStr)
									// 		if err != nil {
									// 			logger.Error("failed to extract the msg section from recorded message", zap.Error(err))
									// 			return
									// 		}
									// 		msgs := strings.Split(msgsStr, ", ")
									// 		docs := []bsoncore.Document{}
									// 		for _, msg := range msgs {
									// 			var unmarshaledDoc bsoncore.Document
									// 			// err = bson.UnmarshalExtJSON([]byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3]), false, &unmarshaledDoc)
									// 			err = bson.UnmarshalExtJSON([]byte(msg), true, &unmarshaledDoc)
									// 			if err != nil {
									// 				logger.Error("failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
									// 				return
									// 			}
									// 			docs = append(docs, unmarshaledDoc)
									// 		}
									// 		// msg.sections = []opMsgSection{&opMsgSectionSingle{msg: []byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3])}}
									// 		// msg.sections = []opMsgSection{&opMsgSectionSingle{
									// 		// 	msg: unmarshaledDoc,
									// 		// }}
									// 		message.sections = append(message.sections, &opMsgSectionSequence{
									// 			// msg: unmarshaledDoc,
									// 			identifier: identifier,
									// 			msgs:       docs,
									// 		})
									// 	} else {
									// 		// sectionStr := strings.Trim(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3], " ")
									// 		// sectionStr := msgSpec.Sections[i][21 : len(msgSpec.Sections[i])-2]
									// 		var sectionStr string
									// 		_, err := fmt.Sscanf(respMessage.Sections[messageIndex], "{ SectionSingle msg: %s }", &sectionStr)
									// 		if err != nil {
									// 			logger.Error("failed to extract the msg section from recorded message single section", zap.Error(err))
									// 			return
									// 		}
									// 		// logger.Info("the section in the msg response", zap.Any("", sectionStr))
									// 		var unmarshaledDoc bsoncore.Document
									// 		// err = bson.UnmarshalExtJSON([]byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3]), false, &unmarshaledDoc)
									// 		err = bson.UnmarshalExtJSON([]byte(sectionStr), true, &unmarshaledDoc)
									// 		if err != nil {
									// 			logger.Error("failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
									// 			return
									// 		}
									// 		// msg.sections = []opMsgSection{&opMsgSectionSingle{msg: []byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3])}}
									// 		// msg.sections = []opMsgSection{&opMsgSectionSingle{
									// 		// 	msg: unmarshaledDoc,
									// 		// }}
									// 		message.sections = append(message.sections, &opMsgSectionSingle{
									// 			msg: unmarshaledDoc,
									// 		})
									// 	}
									// }
									if hasSecondSetBit(respMessage.FlagBits) {
										// the first response wiremessage have
										responseTo := mongoRequests[i].Header.RequestID
										for {
											// fmt.Println("the response cycle. delay: ", resp.ReadDelay, " for clientConnId: ", clientConnId, " responseTo: ", mongoRequests[i].Header.RequestID, " length of mongorequests: ", len(mongoRequests), " the sixteenthBit in request.Msg.Flag: ", hasSixteenthBit(mongoRequests[i].Message.(*models.MongoOpMessage).FlagBits))
											time.Sleep(time.Duration(resp.ReadDelay))
											// generate requestId for the mongo wiremessage
											requestId := wiremessage.NextRequestID()
											_, err := clientConn.Write(message.Encode(responseTo, requestId))
											logger.Debug(fmt.Sprintf("the response lifecycle ended. clientconnid: %v", clientConnId))
											if err != nil {
												logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err))
												return
											}
											// the 'responseTo' field of response wiremessage is set to requestId of currently sent wiremessage
											responseTo = requestId
										}
									} else {

										_, err := clientConn.Write(message.Encode(mongoRequests[i].Header.RequestID, wiremessage.NextRequestID()))
										if err != nil {
											logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err))
											return
										}
									}
									configResponseWritten = true
								}
							}
						default:
							logger.Error("the OpCode of the mongo wiremessage is invalid.")
						}
					}
				}
				if configResponseWritten {
					break
				}
			}
		} else {
			// mockedResponseWritten := false
			maximumMatchingScore := 0.0
			indxOfMostlyMatchedMock := -1
			for tcsIndx, tcsMock := range tcsMocks {
				if len(tcsMock.Spec.MongoRequests) == len(mongoRequests) {
					for i, req := range tcsMock.Spec.MongoRequests {
						if req.Header.Length != mongoRequests[i].Header.Length || req.Header.Opcode != mongoRequests[i].Header.Opcode {
							continue
						}
						switch req.Header.Opcode {
						case wiremessage.OpMsg:
							if req.Message.(*models.MongoOpMessage).FlagBits != mongoRequests[i].Message.(*models.MongoOpMessage).FlagBits {
								continue
							}
							// isMatched := true
							for sectionIndx, section := range req.Message.(*models.MongoOpMessage).Sections {
								// if section != mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx] {

								// if !compareOpMsgSection(section, mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx], logger) {
								// 	logger.Debug("the sections are not matched. ", zap.Any("expected", section), zap.Any("actual", mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx]))
								// 	isMatched = false
								// 	break
								// }
								score := compareOpMsgSection(section, mongoRequests[i].Message.(*models.MongoOpMessage).Sections[sectionIndx], logger)
								if score > maximumMatchingScore {
									maximumMatchingScore = score
									indxOfMostlyMatchedMock = tcsIndx
								}
							}
							// if isMatched {
							// 	for _, resp := range tcsMock.Spec.MongoResponses {
							// 		respMessage := resp.Message.(*models.MongoOpMessage)

							// 		message, err := encodeOpMsg(respMessage, logger)
							// 		if err != nil {
							// 			logger.Error("failed to encode the recorded OpMsg response", zap.Error(err), zap.Any("for request with id", mongoRequests[i].Header.RequestID))
							// 			return
							// 		}
							// 		// if hasSecondSetBit(respMessage.FlagBits) {
							// 		// 	responseTo := mongoRequests[i].Header.RequestID
							// 		// 	for {
							// 		// 		fmt.Println("the response cycle. delay: ", resp.ReadDelay, " for clientConnId: ", clientConnId, " responseTo: ", mongoRequests[i].Header.RequestID, " length of mongorequests: ", len(mongoRequests), " the sixteenthBit in request.Msg.Flag: ", hasSixteenthBit(mongoRequests[i].Message.(*models.MongoOpMessage).FlagBits))
							// 		// 		time.Sleep(time.Duration(resp.ReadDelay))
							// 		// 		requestId := wiremessage.NextRequestID()
							// 		// 		_, err := clientConn.Write(message.Encode(responseTo, requestId))
							// 		// 		logger.Debug(fmt.Sprintf("the response lifecycle ended. clientconnid: %v", clientConnId))
							// 		// 		if err != nil {
							// 		// 			logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err))
							// 		// 			return
							// 		// 		}
							// 		// 		// the 'responseTo' field of response wiremessage is set to requestId of sent r
							// 		// 		responseTo = requestId
							// 		// 		// time.Sleep(10*time.Minute)
							// 		// 	}
							// 		// } else {
							// 		_, err = clientConn.Write(message.Encode(mongoRequests[i].Header.RequestID, wiremessage.NextRequestID()))
							// 		if err != nil {
							// 			logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err), zap.Any("for request with id", mongoRequests[i].Header.RequestID))
							// 			return
							// 		}
							// 		// }
							// 		mockedResponseWritten = true
							// 	}
							// }
						default:
							logger.Error("the OpCode of the mongo wiremessage is invalid.")
						}
					}
				}
				// if mockedResponseWritten {
				// 	break
				// }
			}
			// responseTo := tcsMocks[indxOfMostlyMatchedMock].Spec.MongoRequests[0].Header.RequestID
			responseTo := mongoRequests[0].Header.RequestID
			logger.Debug("the index mostly matched with the current request", zap.Any("indx", indxOfMostlyMatchedMock), zap.Any("responseTo", responseTo))
			for _, resp := range tcsMocks[indxOfMostlyMatchedMock].Spec.MongoResponses {
				respMessage := resp.Message.(*models.MongoOpMessage)

				message, err := encodeOpMsg(respMessage, logger)
				if err != nil {
					logger.Error("failed to encode the recorded OpMsg response", zap.Error(err), zap.Any("for request with id", responseTo))
					return
				}
				// if hasSecondSetBit(respMessage.FlagBits) {
				// 	responseTo := mongoRequests[i].Header.RequestID
				// 	for {
				// 		fmt.Println("the response cycle. delay: ", resp.ReadDelay, " for clientConnId: ", clientConnId, " responseTo: ", mongoRequests[i].Header.RequestID, " length of mongorequests: ", len(mongoRequests), " the sixteenthBit in request.Msg.Flag: ", hasSixteenthBit(mongoRequests[i].Message.(*models.MongoOpMessage).FlagBits))
				// 		time.Sleep(time.Duration(resp.ReadDelay))
				// 		requestId := wiremessage.NextRequestID()
				// 		_, err := clientConn.Write(message.Encode(responseTo, requestId))
				// 		logger.Debug(fmt.Sprintf("the response lifecycle ended. clientconnid: %v", clientConnId))
				// 		if err != nil {
				// 			logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err))
				// 			return
				// 		}
				// 		// the 'responseTo' field of response wiremessage is set to requestId of sent r
				// 		responseTo = requestId
				// 		// time.Sleep(10*time.Minute)
				// 	}
				// } else {
				requestId := wiremessage.NextRequestID()
				_, err = clientConn.Write(message.Encode(responseTo, requestId))
				if err != nil {
					logger.Error("failed to write the health check opmsg to mongo client", zap.Error(err), zap.Any("for request with id", responseTo))
					return
				}
				responseTo = requestId
				// }
				// mockedResponseWritten = true
			}
			logger.Debug(fmt.Sprintf("the length of tcsMocks before filtering matched: %v\n", len(tcsMocks)))
			if maximumMatchingScore > 0.0 && indxOfMostlyMatchedMock >= 0 && indxOfMostlyMatchedMock < len(tcsMocks) {
				tcsMocks = append(tcsMocks[:indxOfMostlyMatchedMock], tcsMocks[indxOfMostlyMatchedMock+1:]...)
				h.SetTcsMocks(tcsMocks)
			}
			logger.Debug(fmt.Sprintf("the length of tcsMocks after filtering matched: %v\n", len(tcsMocks)))
		}
		requestBuffer = []byte("read form client connection")
		// fmt.Printf("an iteration of a mongo request lifecycle is completed. clientconnid: %v\n", clientConnId)
	}
}

// func encodeOutgoingMongo(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) []*models.Mock {
func encodeOutgoingMongo(clientConnId, destConnId int, requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, started time.Time, readRequestDelay time.Duration, logger *zap.Logger) {
	rand.Seed(time.Now().UnixNano())
	// clientConnId := rand.Intn(101)
	for {

		var err error
		var logStr string = fmt.Sprintln("the connection id: ", clientConnId, " the destination conn id: ", destConnId)

		logStr += fmt.Sprintln("started reading from the client: ", started)
		if string(requestBuffer) == "read form client connection" {
			lstr := ""
			started := time.Now()
			requestBuffer, lstr, err = util.ReadBytes1(clientConn)
			if err != nil {
				logger.Error(Emoji+"failed to read request from the mongo client", zap.Error(err), zap.String("mongo client address", clientConn.RemoteAddr().String()), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
				return
			}
			readRequestDelay = time.Since(started)
			logStr += lstr
			logger.Debug(fmt.Sprintf("the request in the mongo parser before passing to dest: %v", len(requestBuffer)), zap.Any("client connId", clientConnId), zap.Any("dest connId", destConnId))
		}

		var (
			mongoRequests  = []models.MongoRequest{}
			mongoResponses = []models.MongoResponse{}
		)
		opReq, requestHeader, mongoRequest, err := Decode(requestBuffer, logger)
		if err != nil {
			logger.Error(Emoji+"failed to decode the mongo wire message from the client", zap.Error(err), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
			return
		}
		mongoRequests = append(mongoRequests, models.MongoRequest{
			Header:    &requestHeader,
			Message:   mongoRequest,
			ReadDelay: int64(readRequestDelay),
		})
		// logStr += fmt.Sprintf("the req buffer: %v\n", string(requestBuffer))
		logStr += fmt.Sprintf("after reading request from client: %v\n", time.Since(started))
		// fmt.Println("the req buffer: ", string(requestBuffer))
		// write the request message to the mongo server
		_, err = destConn.Write(requestBuffer)
		if err != nil {
			logger.Error(Emoji+"failed to write the request buffer to mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
			return
		}
		logger.Debug(fmt.Sprintf("the request in the mongo parser after passing to dest: %v", len(requestBuffer)), zap.Any("client connId", clientConnId), zap.Any("dest connId", destConnId))

		logStr += fmt.Sprintln("after writing the request to the destination: ", time.Since(started))
		if val, ok := mongoRequest.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
			for {
				// fmt.Println("into the more_to_come", logStr)
				tmpStr := ""
				started = time.Now()
				requestBuffer1, tmpStr, err := util.ReadBytes1(clientConn)
				logStr += tmpStr
				if err != nil {
					logger.Error(Emoji+"failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
					return
				}
				readRequestDelay = time.Since(started)

				// logStr += fmt.Sprintf("the resp buffer: %v\n", string(responseBuffer))
				logStr += fmt.Sprintf("after reading the response from destination: %v\n", time.Since(started))
				// fmt.Println("the resp buffer: ", string(responseBuffer))

				// write the reply to mongo client
				_, err = destConn.Write(requestBuffer1)
				if err != nil {
					// fmt.Println(logStr)
					logger.Error(Emoji+"failed to write the reply message to mongo client", zap.Error(err), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
					return
				}

				logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

				if len(requestBuffer1) == 0 {
					logger.Debug("the response from the server is complete")
					break
				}
				// logStr += fmt.Sprintln("the response from the mongo server: ", string(responseBuffer1))
				// fmt.Println("the response from the mongo server: ", string(requestBuffer1))
				_, reqHeader, mongoReq, err := Decode(requestBuffer1, logger)
				if err != nil {
					logger.Error(Emoji+"failed to decode the mongo wire message from the destination server", zap.Error(err), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
					return
				}
				if mongoReqVal, ok := mongoReq.(models.MongoOpMessage); ok && !hasSecondSetBit(mongoReqVal.FlagBits) {
					logger.Debug("the request from the client is complete since the more_to_come flagbit is 0")
					break
				}
				// logStr += fmt.Sprintln("the docoded responseHeader: ", responseHeader, " || and the response: ", mongoResp.(*models.MongoOpMessage))
				// fmt.Println("the docoded responseHeader: ", reqHeader, " || and the response: ", mongoReq.(*models.MongoOpMessage))
				mongoRequests = append(mongoRequests, models.MongoRequest{
					Header:    &reqHeader,
					Message:   mongoReq,
					ReadDelay: int64(readRequestDelay),
				})

			}
		}

		// read reply message from the mongo server
		tmpStr := ""
		started = time.Now()
		responseBuffer, tmpStr, err := util.ReadBytes1(destConn)
		logStr += tmpStr
		if err != nil {
			logger.Error(Emoji+"failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
			return
		}
		readResponseDelay := time.Since(started)
		// logStr += fmt.Sprintf("the resp buffer: %v\n", string(responseBuffer))
		logStr += fmt.Sprintf("after reading the response from destination: %v\n", time.Since(started))
		// fmt.Println("the resp buffer: ", string(responseBuffer))

		// write the reply to mongo client
		_, err = clientConn.Write(responseBuffer)
		if err != nil {
			// fmt.Println(logStr)
			logger.Error(Emoji+"failed to write the reply message to mongo client", zap.Error(err), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
			return
		}

		logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

		_, responseHeader, mongoResponse, err := Decode(responseBuffer, logger)
		if err != nil {
			logger.Error(Emoji+"failed to decode the mongo wire message from the destination server", zap.Error(err), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
			return
		}
		mongoResponses = append(mongoResponses, models.MongoResponse{
			Header:    &responseHeader,
			Message:   mongoResponse,
			ReadDelay: int64(readResponseDelay),
		})
		if val, ok := mongoResponse.(*models.MongoOpMessage); ok && hasSecondSetBit(val.FlagBits) {
			for i := 0; ; i++ {
				// fmt.Printf("the more_to_come is a heartbeat?: %v", isHeartBeat(opReq, *mongoRequests[0].Header, mongoRequests[0].Message))
				if i == 0 && isHeartBeat(opReq, *mongoRequests[0].Header, mongoRequests[0].Message) {
					go recordMessage(h, requestBuffer, responseBuffer, logStr, mongoRequests, mongoResponses, opReq)
				}
				// fmt.Println("into the more_to_come", logStr)
				tmpStr := ""
				started = time.Now()
				responseBuffer, err = util.ReadBytes(destConn)
				logStr += tmpStr
				if err != nil {
					logger.Error(Emoji+"failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
					return
				}
				logger.Debug(fmt.Sprintf("the response in the mongo parser before passing to client: %v", len(responseBuffer)), zap.Any("client connId", clientConnId), zap.Any("dest connId", destConnId))

				readResponseDelay := time.Since(started)

				// logStr += fmt.Sprintf("the resp buffer: %v\n", string(responseBuffer))
				logStr += fmt.Sprintf("after reading the response from destination: %v\n", time.Since(started))
				// fmt.Println("the resp buffer: ", string(responseBuffer))

				// write the reply to mongo client
				_, err = clientConn.Write(responseBuffer)
				if err != nil {
					// fmt.Println(logStr)
					logger.Error(Emoji+"failed to write the reply message to mongo client", zap.Error(err), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
					return
				}
				logger.Debug(fmt.Sprintf("the response in the mongo parser after passing to client: %v", len(responseBuffer)), zap.Any("client connId", clientConnId), zap.Any("dest connId", destConnId))

				logStr += fmt.Sprintln("after writting response to the client: ", time.Since(started), "current time is: ", time.Now())

				if len(responseBuffer) == 0 {
					logger.Debug("the response from the server is complete")
					break
				}
				// logStr += fmt.Sprintln("the response from the mongo server: ", string(responseBuffer1))
				// fmt.Println("the response from the mongo server: ", string(responseBuffer1))
				_, respHeader, mongoResp, err := Decode(responseBuffer, logger)
				if err != nil {
					logger.Error(Emoji+"failed to decode the mongo wire message from the destination server", zap.Error(err), zap.Any("client conn id", clientConnId), zap.Any("dest conn id", destConnId))
					return
				}
				if mongoRespVal, ok := mongoResp.(models.MongoOpMessage); ok && !hasSecondSetBit(mongoRespVal.FlagBits) {
					logger.Debug("the response from the server is complete since the more_to_come flagbit is 0")
					break
				}
				// logStr += fmt.Sprintln("the docoded responseHeader: ", responseHeader, " || and the response: ", mongoResp.(*models.MongoOpMessage))
				// fmt.Println("the docoded responseHeader: ", respHeader, " || and the response: ", mongoResp.(*models.MongoOpMessage))
				mongoResponses = append(mongoResponses, models.MongoResponse{
					Header:    &respHeader,
					Message:   mongoResp,
					ReadDelay: int64(readResponseDelay),
				})
				// go recordMessage(h, requestBuffer, responseBuffer, logStr, mongoRequests, mongoResponses, opReq)
			}
			// fmt.Println("exiting the more_to_come")
		}

		go recordMessage(h, requestBuffer, responseBuffer, logStr, mongoRequests, mongoResponses, opReq)
		requestBuffer = []byte("read form client connection")

	}

}

func recordMessage(h *hooks.Hook, requestBuffer, responseBuffer []byte, logStr string, mongoRequests []models.MongoRequest, mongoResponses []models.MongoResponse, opReq Operation) {
	// fmt.Println(logStr)
	// fmt.Println("the resquest buffer in the go routine: ", string(requestBuffer))
	// fmt.Println("the response buffer in the go routine: ", string(responseBuffer))

	// // capture if the wiremessage is a mongo operation call

	shouldRecordCalls := true
	name := "mocks"

	// Skip heartbeat from capturing in the global set of mocks. Since, the heartbeat packet always contain the "hello" boolean.
	// See: https://github.com/mongodb/mongo-go-driver/blob/8489898c64a2d8c2e2160006eb851a11a9db9e9d/x/mongo/driver/operation/hello.go#L503
	// if strings.Contains(opReq.String(), "helloOk") || strings.Contains(opReq.String(), "hello") {
	if isHeartBeat(opReq, *mongoRequests[0].Header, mongoRequests[0].Message) {
		name = "config"
		// isHeartbeatRecorded := false
		for _, v := range configRequests {
			// requestHeader.
			for _, req := range mongoRequests {

				switch req.Header.Opcode {
				case wiremessage.OpQuery:
					if req.Message.(*models.MongoOpQuery).Query == v {
						shouldRecordCalls = false
						break
					}
					configRequests = append(configRequests, req.Message.(*models.MongoOpQuery).Query)
				case wiremessage.OpMsg:
					if len(req.Message.(*models.MongoOpMessage).Sections) > 0 && req.Message.(*models.MongoOpMessage).Sections[0] == v {
						shouldRecordCalls = false
						break
					}
					configRequests = append(configRequests, req.Message.(*models.MongoOpMessage).Sections[0])
				default:
					if opReq.String() == v {
						shouldRecordCalls = false
						break
					}
					configRequests = append(configRequests, opReq.String())

				}
			}
		}
	}
	if shouldRecordCalls {
		meta1 := map[string]string{
			"operation": opReq.String(),
		}
		mongoMock := &models.Mock{
			Version: models.V1Beta2,
			Kind:    models.Mongo,
			Name:    name,
			Spec: models.MockSpec{
				Metadata:       meta1,
				MongoRequests:  mongoRequests,
				MongoResponses: mongoResponses,
				Created:        time.Now().Unix(),
			},
		}
		h.AppendMocks(mongoMock)
	}
}

func hasSecondSetBit(num int) bool {
	// Shift the number right by 1 bit and check if the least significant bit is set
	return (num>>1)&1 == 1
}

func hasSixteenthBit(num int) bool {
	// Shift the number right by 1 bit and check if the least significant bit is set
	return (num>>16)&1 == 1
}

// Skip heartbeat from capturing in the global set of mocks. Since, the heartbeat packet always contain the "hello" boolean.
// See: https://github.com/mongodb/mongo-go-driver/blob/8489898c64a2d8c2e2160006eb851a11a9db9e9d/x/mongo/driver/operation/hello.go#L503
func isHeartBeat(opReq Operation, requestHeader models.MongoHeader, mongoRequest interface{}) bool {

	switch requestHeader.Opcode {
	case wiremessage.OpQuery:
		val, ok := mongoRequest.(*models.MongoOpQuery)
		if ok {
			return val.FullCollectionName == "admin.$cmd" && opReq.IsIsMaster() && strings.Contains(opReq.String(), "helloOk")
		}
	case wiremessage.OpMsg:
		// fmt.Printf("check that the opmsg is heartbeat or not. operation: %v\n", opReq.String())
		_, ok := mongoRequest.(*models.MongoOpMessage)
		if ok {
			// fmt.Printf("the request opmg is isMaster? : %v \n", opReq.IsIsAdminDB())
			return opReq.IsIsAdminDB() && strings.Contains(opReq.String(), "hello")
		}
	default:
		return false
	}
	return false
}

func compareOpMsgSection(expectedSection, actualSection string, logger *zap.Logger) float64 {
	// a := map[string]interface{}{}
	// b := map[string]interface{}{}
	// check that the sections are of same type. SectionSingle (section[16] is "m") or SectionSequence (section[16] is "i").
	if (len(expectedSection) < 16 || len(actualSection) < 16) && expectedSection[16] != actualSection[16] {
		return 0
	}
	logger.Debug(fmt.Sprintf("the sections are. Expected: %v\n and actual: %v", expectedSection, actualSection))
	switch {
	case strings.HasPrefix(expectedSection, "{ SectionSingle identifier:"):
		var expectedIdentifier string
		var expectedMsgsStr string
		// // Define the regular expression pattern
		// pattern := `\{ SectionSingle identifier: (.+?) , msgs: \[ (.+?) \] \}`

		// // Compile the regular expression
		// regex := regexp.MustCompile(pattern)

		// // Find submatches using the regular expression
		// submatches := regex.FindStringSubmatch(expectedSection)
		// if submatches == nil || len(submatches) != 3 {
		// 	logger.Debug(fmt.Sprintf("the section in mongo OpMsg wiremessage: %v", expectedSection))
		// 	logger.Error("failed to fetch the identifier/msgs from the section sequence of recorded OpMsg", zap.Any("identifier", expectedIdentifier))
		// 	return 0
		// }
		// expectedIdentifier = submatches[1]
		// expectedMsgsStr = submatches[2]

		// _, err := fmt.Sscanf(expectedSection, "{ SectionSingle identifier: %s , msgs: [ %s ] }", &expectedIdentifier, &expectedMsgsStr)
		expectedIdentifier, expectedMsgsStr, err := decodeOpMsgSectionSequence(expectedSection)
		if err != nil {
			logger.Debug(fmt.Sprintf("the section in mongo OpMsg wiremessage: %v", expectedSection))
			logger.Error("failed to fetch the identifier/msgs from the section sequence of recorded OpMsg", zap.Error(err), zap.Any("identifier", expectedIdentifier))
			return 0
		}

		var actualIdentifier string
		var actualMsgsStr string
		// _, err = fmt.Sscanf(actualSection, "{ SectionSingle identifier: %s , msgs: [ %s ] }", &actualIdentifier, &actualMsgsStr)
		actualIdentifier, actualMsgsStr, err = decodeOpMsgSectionSequence(actualSection)
		if err != nil {
			logger.Error("failed to fetch the identifier/msgs from the section sequence of incoming OpMsg", zap.Error(err), zap.Any("identifier", actualIdentifier))
			return 0
		}

		// // Compile the regular expression
		// regex = regexp.MustCompile(pattern)

		// // Find submatches using the regular expression
		// submatches = regex.FindStringSubmatch(actualSection)
		// if submatches == nil || len(submatches) != 3 {
		// 	logger.Debug(fmt.Sprintf("the section in mongo OpMsg wiremessage: %v", expectedSection))
		// 	logger.Error("failed to fetch the identifier/msgs from the section sequence of incoming OpMsg", zap.Any("identifier", actualIdentifier))
		// 	return 0
		// }
		// actualIdentifier = submatches[1]
		// actualMsgsStr = submatches[2]

		logger.Debug("the expected section", zap.Any("identifier", expectedIdentifier), zap.Any("docs", expectedMsgsStr))
		logger.Debug("the actual section", zap.Any("identifier", actualIdentifier), zap.Any("docs", actualMsgsStr))

		expectedMsgs := strings.Split(expectedMsgsStr, ", ")
		actualMsgs := strings.Split(actualMsgsStr, ", ")
		if len(expectedMsgs) != len(actualMsgs) || expectedIdentifier != actualIdentifier {
			return 0
		}
		score := 0.0
		for i := range expectedMsgs {
			expected := map[string]interface{}{}
			actual := map[string]interface{}{}
			err := bson.UnmarshalExtJSON([]byte(expectedMsgs[i]), true, &expected)
			// err := json.Unmarshal([]byte(string1), &a)
			if err != nil {
				logger.Error(fmt.Sprintf("failed to unmarshal the section of recorded request to bson document"), zap.Error(err))
				return 0
			}
			err = bson.UnmarshalExtJSON([]byte(actualMsgs[i]), true, &actual)
			if err != nil {
				logger.Error(fmt.Sprintf("failed to unmarshal the section of incoming request to bson document"), zap.Error(err))
				return 0
			}
			score += calculateMatchingScore(expected, actual)
			// if diff := deep.Equal(expected, actual); diff != nil {
			// 	for _, v := range diff {
			// 		// lsid is used for keeping track of the session sent by the mongo client in operation calls
			// 		if !strings.Contains(v, "map[lsid].map[id]: {") {
			// 			return false
			// 		}
			// 	}
			// }
		}
		logger.Debug("the matching score for sectionSequence", zap.Any("", score))
		return score
	case strings.HasPrefix(expectedSection, "{ SectionSingle msg:"):
		var expectedMsgsStr string
		expectedMsgsStr, err := decodeOpMsgSectionSingle(actualSection)
		if err != nil {
			logger.Error("failed to fetch the msgs from the single section of recorded OpMsg", zap.Error(err))
			return 0
		}
		// // Define the regular expression pattern
		// pattern := `\{ SectionSingle msg: (.+?) \}`

		// // Compile the regular expression
		// regex := regexp.MustCompile(pattern)

		// // Find submatches using the regular expression
		// submatches := regex.FindStringSubmatch(expectedSection)
		// if submatches == nil || len(submatches) != 2 {
		// 	logger.Debug(fmt.Sprintf("the section in mongo OpMsg wiremessage: %v", expectedSection))
		// 	logger.Error("failed to fetch the identifier/msgs from the section sequence of recorded OpMsg")
		// 	return 0
		// }
		// // expectedIdentifier = submatches[1]
		// expectedMsgsStr = submatches[1]

		// _, err := fmt.Sscanf(expectedSection, "{ SectionSingle msg: %s }", &expectedMsgsStr)
		// if err != nil {
		// 	logger.Error("failed to fetch the msgs from the single section of recorded OpMsg", zap.Error(err))
		// 	return 0
		// }

		var actualMsgsStr string
		actualMsgsStr, err = decodeOpMsgSectionSingle(actualSection)
		if err != nil {
			logger.Error("failed to fetch the msgs from the single section of incoming OpMsg", zap.Error(err))
			return 0
		}
		// // Compile the regular expression
		// regex = regexp.MustCompile(pattern)

		// // Find submatches using the regular expression
		// submatches = regex.FindStringSubmatch(actualSection)
		// if submatches == nil || len(submatches) != 2 {
		// 	logger.Debug(fmt.Sprintf("the section in mongo OpMsg wiremessage: %v", expectedSection))
		// 	logger.Error("failed to fetch the identifier/msgs from the section sequence of recorded OpMsg")
		// 	return 0
		// }
		// // expectedIdentifier = submatches[1]
		// actualMsgsStr = submatches[1]
		// _, err = fmt.Sscanf(actualSection, "{ SectionSingle msg: %s }", &actualMsgsStr)
		// if err != nil {
		// 	logger.Error("failed to fetch the msgs from the single section of incoming OpMsg", zap.Error(err))
		// 	return 0
		// }

		expected := map[string]interface{}{}
		actual := map[string]interface{}{}

		err = bson.UnmarshalExtJSON([]byte(expectedMsgsStr), true, &expected)
		// err := json.Unmarshal([]byte(string1), &expected)
		if err != nil {
			logger.Error(fmt.Sprintf("failed to unmarshal the section of recorded request to bson document"), zap.Error(err))
			return 0
		}
		err = bson.UnmarshalExtJSON([]byte(actualMsgsStr), true, &actual)
		if err != nil {
			logger.Error(fmt.Sprintf("failed to unmarshal the section of incoming request to bson document"), zap.Error(err))
			return 0
		}
		logger.Debug("the expected and actual msg in the single section.", zap.Any("expected", expected), zap.Any("actual", actual), zap.Any("score", calculateMatchingScore(expected, actual)))
		return calculateMatchingScore(expected, actual)
		// if diff := deep.Equal(expected, actual); diff != nil {
		// 	logger.Debug(fmt.Sprintf("the diff is: %v, len(diff): %v. bool: %v", diff, len(diff), !strings.Contains(diff[0], "map[lsid].map[id]: {")))
		// 	for _, v := range diff {
		// 		// lsid is used for keeping track of the session sent by the mongo client in operation calls
		// 		if !strings.Contains(v, "map[lsid].map[id]: {") {
		// 			logger.Debug("the difference is not lsid ", zap.Any("", diff))
		// 			return false
		// 		}
		// 	}
		// }
	default:
		logger.Error(fmt.Sprintf("failed to detect the OpMsg section into mongo request wiremessage due to invalid format"))
		return 0
	}
}

func calculateMatchingScore(obj1, obj2 map[string]interface{}) float64 {
	totalFields := len(obj2)
	matchingFields := 0.0

	for key, value := range obj2 {
		if obj1Value, ok := obj1[key]; ok {
			if reflect.DeepEqual(value, obj1Value) {
				matchingFields++
			} else if reflect.TypeOf(value) == reflect.TypeOf(obj1Value) {
				if isNestedMap(value) {
					if isNestedMap(obj1Value) {
						matchingFields += calculateMatchingScore(obj1Value.(map[string]interface{}), value.(map[string]interface{}))
					}
				} else if isSlice(value) {
					if isSlice(obj1Value) {
						matchingFields += calculateMatchingScoreForSlices(obj1Value.([]interface{}), value.([]interface{}))
					}
				}
			}
		}
	}

	return float64(matchingFields) / float64(totalFields)
}

func calculateMatchingScoreForSlices(slice1, slice2 []interface{}) float64 {
	matchingCount := 0

	if len(slice1) == len(slice2) {
		for indx2, item2 := range slice2 {
			if len(slice1) > indx2 && reflect.DeepEqual(item2, slice1[indx2]) {
				matchingCount++
			}
		}
	}

	return float64(matchingCount) / float64(len(slice2))
}

func isNestedMap(value interface{}) bool {
	_, ok := value.(map[string]interface{})
	return ok
}

func isSlice(value interface{}) bool {
	_, ok := value.([]interface{})
	return ok
}
