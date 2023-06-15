package mongoparser

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/x/bsonx/bsoncore"
	"go.mongodb.org/mongo-driver/x/mongo/driver/wiremessage"
	"go.uber.org/zap"
)

// IsOutgoingMongo function determines if the outgoing network call is Mongo by comparing the
// message format with that of a mongo wire message.
func IsOutgoingMongo(buffer []byte) bool {
	if len(buffer) < 4 {
		return false
	}
	messageLength := binary.LittleEndian.Uint32(buffer[0:4])
	return int(messageLength) == len(buffer)
}

func ProcessOutgoingMongo (requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	switch models.GetMode() {
	case models.MODE_RECORD:
		capturedDeps := encodeOutgoingMongo(requestBuffer, clientConn, destConn, logger)
		// *deps = append(*deps, capturedDeps...)
		for _, v := range capturedDeps {
			h.AppendDeps(v)
		}
	case models.MODE_TEST:
		decodeOutgoingMongo(requestBuffer, clientConn, destConn, h, logger)
	default:
	}
}

func decodeOutgoingMongo(requestBuffer []byte, clientConn, destConn net.Conn, h *hooks.Hook, logger *zap.Logger) {
	// var helloReply, replyMsg = []byte{}, []byte{}
		// _, _, _, _, _, err := Decode(buffer)
		_, requestHeader, _, err := Decode(requestBuffer)
		if err != nil {
			logger.Error("failed to decode the mongo request wiremessage", zap.Error(err))
			return
			// log.Println("failed to decode the mongo wire message", err)
			// break
		}
	
		// if len(deps) <= 1 {
		if h.GetDepsSize() <= 1 {
			// logger.Error("failed to mock the output for unrecorded outgoing mongo query")
			// log.Println("the dep call is not mocked during record")
			return
		// } else {
			// 	helloReply = deps[0].Spec.Objects[1].Data
			// 	replyMsg = deps[1].Spec.Objects[1].Data
			}
			// fmt.Println("deps: ", *deps[0], "\n", *deps[1], "\n\n ")
			// mongoSpec := deps[0]
			mongoSpec := h.FetchDep(0)
		// var mongoSpec spec.MongoSpec
		// err = deps[0].Spec.Decode(&mongoSpec)
		// if err != nil {
		// 	logger.Error("failed to decode the yaml spec for the outgoing mongo call")
		// 	return
		// }
		// fmt.Printf("mongoSpec: %v\n", mongoSpec)

		// querySpec, ok := mongoSpec.Spec.MongoRequest.(models.MongoOpQuery)
		// if !ok {
		// 	logger.Error("failed to decode the yaml spec for the outgoing mongo request")
		// 	return
		// }
		// var querySpec spec.MongoOpQuery
		// err = mongoSpec.Request.Decode(&querySpec)
		// if err != nil {
		// 	logger.Error("failed to decode the yaml spec for the outgoing mongo request", ap.Error(err))
		// 	return
		// }

		replySpec, ok := mongoSpec.Spec.MongoResponse.(*models.MongoOpReply)
		if !ok {
			logger.Error("failed to decode the yaml for mongo OpReply wiremessage")
			return
		}

		// var replySpec spec.MongoOpReply = spec.MongoOpReply{}
		// err = mongoSpec.Response.Decode(&replySpec)
		// if err != nil {
		// 	logger.Error("failed to decode the yaml spec for the outgoing mongo reply")
		// 	return
		// }
		// println("the replyspec is: ", replySpec.ResponseFlags)

		// opReply
		replyDocs := []bsoncore.Document{}
		for _, v := range replySpec.Documents {
			var unmarshaledDoc bsoncore.Document
			err = bson.UnmarshalExtJSON([]byte(v), false, &unmarshaledDoc)
			if err != nil {
				logger.Error("failed to unmarshal the recorded document string of OpReply", zap.Error(err))
				return
			}
			
			// docs, rm, ok := bsoncore.ReadDocument([]byte(v))
			// fmt.Println("the document in healtcheck of test mode: ", docs.String(), " rm bytes: ", rm,  " ok: ", ok)
			// replyDocs = append(replyDocs, bsoncore.Document(v))
			replyDocs = append(replyDocs, unmarshaledDoc)

			// fmt.Println("the documents in healtcheck of test mode: ", replyDocs)
		}
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
		var heathCheckReplyBuffer []byte
		heathCheckReplyBuffer = wiremessage.AppendHeader(heathCheckReplyBuffer, mongoSpec.Spec.MongoResponseHeader.Length, requestHeader.RequestID, requestHeader.ResponseTo, mongoSpec.Spec.MongoResponseHeader.Opcode)
		heathCheckReplyBuffer = wiremessage.AppendReplyFlags(heathCheckReplyBuffer, wiremessage.ReplyFlag(replySpec.ResponseFlags))
		heathCheckReplyBuffer = wiremessage.AppendReplyCursorID(heathCheckReplyBuffer, replySpec.CursorID)
		heathCheckReplyBuffer = wiremessage.AppendReplyStartingFrom(heathCheckReplyBuffer, replySpec.StartingFrom)
		heathCheckReplyBuffer = wiremessage.AppendReplyNumberReturned(heathCheckReplyBuffer, replySpec.NumberReturned)
		for _, doc := range replyDocs {
			heathCheckReplyBuffer = append(heathCheckReplyBuffer, doc...)
		}
		// heathCheckReplyBuffer = append(heathCheckReplyBuffer, reply.Encode(requestHeader.ResponseTo)...)

		// opr, _, _, err = Decode(heathCheckReplyBuffer)
		// println("the healthcheck response : ", opr.String())

		_, err = clientConn.Write(heathCheckReplyBuffer)
		if err != nil {
			logger.Error("failed to write the health check reply to mongo client", zap.Error(err))
			return
			// log.Printf("failed to write response to the client conn. error: %v \n", err.Error())
		}
	
		// _, _, _, _, _, err = Decode(helloReply)
		// if err != nil {
		// 	log.Println("failed to decode the mongo wire message", err)
		// 	// break
		// }
		// if !strings.Contains(op.String(), "hello") {
		// 	log.Printf("the decoded response wire message: length: %v, reqId: %v, respTo: %v, opCode: %v, body: %v on mode: %v", l, reqId, respTo, opCode, op.String(), os.Getenv("KEPLOY_MODE"))
		// }
	
		operationBuffer, err := util.ReadBytes(clientConn)
		if err != nil {
			logger.Error("faile to read the mongo wiremessage for operation query", zap.Error(err))
			return
			// log.Printf("failed to read the message. error: %v\n", err) 
			// break
		}
	
		
		opr1, _, _, err := Decode(operationBuffer)
		if err != nil {
			logger.Error("failed to decode the mongo operation query", zap.Error(err))
			// panic("stop due to invalid mongo wiremessage")
			return
			// log.Printf("failed to decode the mongo wire message. error: %v", err.Error())
			// break
		}
		// panic("stop recuring loop")
		if strings.Contains(opr1.String(), "hello") {

			return
		}
	
		// if !strings.Contains(opr1.String(), "hello") {
		// 	log.Printf("the decoded wire message: length: %v, reqId: %v, respTo: %v, opCode: %v, body: %v on mode: %v", lr, reqIdr, respTor, opCoder, opr1.String(), os.Getenv("KEPLOY_MODE"))
		// }
	
		// log.Println("length of deps: ", len(deps))
		// log.Println(", length of objects: ", len(deps[1].Spec.Objects))
		// mongoSpec1 := deps[1]
		mongoSpec1 := h.FetchDep(1)
		// 	var mongoSpec1 spec.MongoSpec
		// err = deps[1].Spec.Decode(&mongoSpec1)
		// if err != nil {
		// 	logger.Error("failed to decode the yaml spec for the outgoing mongo call")
		// 	return
		// }

		// msgQuerySpec, ok := mongoSpec1.Spec.MongoRequest.(models.MongoOpMessage)
		// if !ok {
		// 	logger.Error("failed to decode the yaml for mongo OpMessage wiremessage request")
		// 	return
		// }

		// var msgQuerySpec spec.MongoOpMessage
		// err = mongoSpec1.Request.Decode(&msgQuerySpec)
		// if err != nil {
		// 	logger.Error("failed to decode the yaml spec for the outgoing mongo request")
		// 	return
		// }

		msgSpec, ok := mongoSpec1.Spec.MongoResponse.(*models.MongoOpMessage)

		// fmt.Println("mock for the opmsg: ", mongoSpec1.Spec.MongoResponse, "\n ")
		if !ok {
			logger.Error("failed to decode the yaml for mongo OpMessage wiremessage response")
			return
		}

		// var msgSpec spec.MongoOpMessage
		// err = mongoSpec1.Response.Decode(&msgSpec)
		// if err != nil {
		// 	logger.Error("failed to decode the yaml spec for the outgoing mongo reply")
		// 	return
		// }
		// fmt.Println("the msg spec: ", msgSpec)
// wiremessage.

		msg := &opMsg{
			flags: wiremessage.MsgFlag(msgSpec.FlagBits),
			checksum: uint32(msgSpec.Checksum),
			sections: []opMsgSection{},
		}

		// if len(msgSpec.Sections) == 1 {
			
		// 	// sectionStr := strings.Trim(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3], " ")
		// 	// sectionStr := msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-2]

		// 	var sectionStr string
		// 	_, err := fmt.Sscanf(msgSpec.Sections[0], "{ SectionSingle msg: %s }", &sectionStr)
		// 	if err != nil {
		// 		logger.Error("failed to scan the message section from the recorded section string", zap.Error(err))
		// 		return
		// 	}

		// 	// logger.Info("the section in the msg response", zap.Any("", sectionStr))
		// 	var unmarshaledDoc bsoncore.Document
		// 	// err = bson.UnmarshalExtJSON([]byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3]), false, &unmarshaledDoc)
		// 	err = bson.UnmarshalExtJSON([]byte(sectionStr), false, &unmarshaledDoc)
		// 	if err != nil {
		// 		logger.Error("failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
		// 		return
		// 	}
		// 	// fmt.Println("the unmarshaled doc: ", unmarshaledDoc)
		// 	// msg.sections = []opMsgSection{&opMsgSectionSingle{msg: []byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3])}}
		// 	msg.sections = []opMsgSection{&opMsgSectionSingle{
		// 		msg: unmarshaledDoc,
		// 	}}
		// } else {
			for i, v := range msgSpec.Sections {
				if strings.Contains(v, "SectionSingle identifier") {
					// sectionStr := strings.Trim(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3], " ")
					// sectionStr := msgSpec.Sections[i][21 : len(msgSpec.Sections[i])-2]
					// logger.Info("the section in the msg response", zap.Any("", sectionStr))

					var identifier string
					var msgsStr string
					_, err := fmt.Sscanf(msgSpec.Sections[i], "{ SectionSingle identifier: %s, msgs: [%s] }", &identifier, &msgsStr)
					if err != nil {
						logger.Error("failed to extract the msg section from recorded message", zap.Error(err))
						return
					}
					msgs := strings.Split(msgsStr, ", ")
					docs := []bsoncore.Document{}
					for _, v := range msgs {
						var unmarshaledDoc bsoncore.Document
						// err = bson.UnmarshalExtJSON([]byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3]), false, &unmarshaledDoc)
						err = bson.UnmarshalExtJSON([]byte(v), false, &unmarshaledDoc)
						if err != nil {
							logger.Error("failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
							return
						}
						docs = append(docs, unmarshaledDoc)
					}
					// msg.sections = []opMsgSection{&opMsgSectionSingle{msg: []byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3])}}
					// msg.sections = []opMsgSection{&opMsgSectionSingle{
					// 	msg: unmarshaledDoc,
					// }}
					msg.sections = append(msg.sections, &opMsgSectionSequence{
						// msg: unmarshaledDoc,
						identifier: identifier,
						msgs: docs,
					})
				} else {
					// sectionStr := strings.Trim(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3], " ")
					// sectionStr := msgSpec.Sections[i][21 : len(msgSpec.Sections[i])-2]

					var sectionStr string
					_, err := fmt.Sscanf(msgSpec.Sections[0], "{ SectionSingle msg: %s }", &sectionStr)
					if err != nil {
						logger.Error("failed to extract the msg section from recorded message single section", zap.Error(err))
						return
					}

					// logger.Info("the section in the msg response", zap.Any("", sectionStr))
					var unmarshaledDoc bsoncore.Document
					// err = bson.UnmarshalExtJSON([]byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3]), false, &unmarshaledDoc)
					err = bson.UnmarshalExtJSON([]byte(sectionStr), false, &unmarshaledDoc)
					if err != nil {
						logger.Error("failed to unmarshal the recorded document string of OpMsg", zap.Error(err))
						return
					}
					// msg.sections = []opMsgSection{&opMsgSectionSingle{msg: []byte(msgSpec.Sections[0][21 : len(msgSpec.Sections[0])-3])}}
					// msg.sections = []opMsgSection{&opMsgSectionSingle{
					// 	msg: unmarshaledDoc,
					// }}
					msg.sections = append(msg.sections, &opMsgSectionSingle{
						msg: unmarshaledDoc,
					})		
				}
			}
		// }
		
		_, err = clientConn.Write(msg.Encode(mongoSpec1.Spec.MongoRequestHeader.ResponseTo))
		if err != nil {
			logger.Error("failed to write the OpMsg response for the mongo operation", zap.Error(err))
			return
			// log.Printf("failed to write response to the client conn. error: %v \n", err.Error())
		}
	
		// _, _, _, _, _, err = Decode(replyMsg)
		// if err != nil {
		// 	log.Println("failed to decode the mongo wire message", err)
		// 	// break
		// }
		// if !strings.Contains(op.String(), "hello") {
		// 	log.Printf("the decoded response wire message: length: %v, reqId: %v, respTo: %v, opCode: %v, body: %v on mode: %v", l, reqId, respTo, opCode, op.String(), os.Getenv("KEPLOY_MODE"))
		// }
		// deps = deps[2:]
		h.PopFront()
		h.PopFront()
}

func encodeOutgoingMongo(requestBuffer []byte, clientConn, destConn net.Conn, logger *zap.Logger) []*models.Mock {

	// write the request message to the mongo server
	_, err := destConn.Write(requestBuffer)
	if err != nil {
		logger.Error("failed to write the request buffer to mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
		return nil
	}

	// read reply message from the mongo server
	responseBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error("failed to read reply from the mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
		return nil
	}

	// write the reply to mongo client
	_, err = clientConn.Write(responseBuffer)
	if err != nil {
		logger.Error("failed to write the reply message to mongo client", zap.Error(err))
		return nil
	}

	// read the operation request message from the mongo client
	msgRequestbuffer, err := util.ReadBytes(clientConn)
	if err != nil {
		logger.Error("failed to read the message from the mongo client", zap.Error(err))
		return nil
	}

	opr1, _, _, err := Decode(msgRequestbuffer)
	if err != nil {
		// logger.Error("failed to decode t")
		return nil
	}

	// write the request message to mongo server
	_, err = destConn.Write(msgRequestbuffer)
	if err != nil {
		logger.Error("failed to write the request message to mongo server", zap.Error(err), zap.String("mongo server address", destConn.LocalAddr().String()))
		return nil
	}

	// read the response message form the mongo server
	msgResponseBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error("failed to read the response message from mongo server", zap.Error(err), zap.String("mongo server address", destConn.RemoteAddr().String()))
		return nil
	}

	// write the response message to mongo client
	_, err = clientConn.Write(msgResponseBuffer)
	if err != nil {
		logger.Error("failed to write the response wiremessage to mongo client", zap.Error(err))
		return nil
	}

	// capture if the wiremessage is a mongo operation call
	if !strings.Contains(opr1.String(), "hello") {
		deps := []*models.Mock{}

		// decode the mongo binary request wiremessage
		opr, requestHeader, mongoRequest, err := Decode((requestBuffer))
		if err != nil {
			logger.Error("failed tp decode the mongo wire message from the client", zap.Error(err))
			return nil
		}

		// decode the mongo binary response wiremessage
		op, responseHeader, mongoResp, err := Decode(responseBuffer)
		if err != nil {
			logger.Error("failed to decode the mongo wire message from the destination server", zap.Error(err))
			return nil
		}

		replyDocs := []string{}
		for _, v := range op.(*opReply).documents {
			replyDocs = append(replyDocs, v.String())
		}
		meta1 := map[string]string{
			"operation": opr.String(),
		}
		mongoMock := &models.Mock{
			Version: models.V1Beta2,
			Kind:    models.Mongo,
			Name:    "",
			Spec: models.MockSpec{
				Metadata: meta1,
				MongoRequestHeader: &requestHeader,
				MongoResponseHeader: &responseHeader,
				MongoRequest: mongoRequest,
				MongoResponse: mongoResp,
				Created: time.Now().Unix(),
			},
		}
		// mongoSpec := &spec.MongoSpec{
		// 	Metadata: meta1,
		// 	RequestHeader: requestHeader,
		// 	ResponseHeader: responseHeader,
		// }
		// err = mongoSpec.Request.Encode(mongoRequest)
		// if err != nil {
		// 	logger.Error("failed to encode the request mongo wiremessage into yaml doc", zap.Error(err))
		// 	return nil
		// }
		// err = mongoSpec.Response.Encode(mongoResp)
		// if err != nil {
		// 	logger.Error("failed to encode the response mongo wiremessage into yaml doc", zap.Error(err))
		// 	return nil
		// }
		// mongoMock.Spec.Encode(mongoSpec)
		deps = append(deps, mongoMock)

		meta := map[string]string{
			"operation": opr1.String(),
		}

		opr, msgRequestHeader, mongoMsgRequest, err := Decode((msgRequestbuffer))
		if err != nil {
			logger.Error("failed tp decode the mongo wire message from the client", zap.Error(err))
			return nil
		}

		op, msgResponseHeader, mongoMsgResponse, err := Decode(msgResponseBuffer)
		if err != nil {
			logger.Error("failed to decode the mongo wire message from the destination server", zap.Error(err))
			return nil
		}
		mongoMock = &models.Mock{
			Version: models.V1Beta2,
			Kind:    models.Mongo,
			Name:    "",
			Spec: models.MockSpec{
				Metadata: meta,
				MongoRequestHeader: &msgRequestHeader,
				MongoResponseHeader: &msgResponseHeader,
				MongoRequest: mongoMsgRequest,
				MongoResponse: mongoMsgResponse,
				Created: time.Now().Unix(),
			},
		}
		// mongoSpec = &spec.MongoSpec{
		// 	Metadata: meta,
		// 	RequestHeader: msgRequestHeader,
		// 	ResponseHeader: msgResponseHeader,
		// }
		// err = mongoSpec.Request.Encode(mongoMsgRequest)
		// if err != nil {
		// 	logger.Error("failed to encode the request mongo wiremessage into yaml doc", zap.Error(err))
		// 	return nil
		// }
		// err = mongoSpec.Response.Encode(mongoMsgResponse)
		// if err != nil {
		// 	logger.Error("failed to encode the response mongo wiremessage into yaml doc", zap.Error(err))
		// 	return nil
		// }
		// mongoMock.Spec.Encode(mongoSpec)
		deps = append(deps, mongoMock)
		return deps
	}

	return nil
}
