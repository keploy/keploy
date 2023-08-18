package models

import( 
	"github.com/jackc/pgproto3/v2"
)

const ProtocolVersionNumber uint32 = 196608 // Replace with actual version number if different

type Packet interface{}

type Backend struct {
	Identfier string `json:"identifier,omitempty"`
	Length uint32 `json:"length,omitempty"`
	Payload string `json:"payload,omitempty"`
	bind           pgproto3.Bind
	cancelRequest  pgproto3.CancelRequest
	_close         pgproto3.Close
	copyFail       pgproto3.CopyFail
	copyData       pgproto3.CopyData
	copyDone       pgproto3.CopyDone
	describe       pgproto3.Describe
	execute        pgproto3.Execute
	flush          pgproto3.Flush
	functionCall   pgproto3.FunctionCall
	gssEncRequest  pgproto3.GSSEncRequest
	parse          pgproto3.Parse
	query          pgproto3.Query
	sslRequest     pgproto3.SSLRequest
	startupMessage pgproto3.StartupMessage
	sync           pgproto3.Sync
	terminate      pgproto3.Terminate
	
}
type Frontend struct {
	Identfier string `json:"identifier,omitempty"`
	Length uint32 `json:"length,omitempty"`
	Payload string `json:"payload,omitempty"`
	authenticationOk                pgproto3.AuthenticationOk
	authenticationCleartextPassword pgproto3.AuthenticationCleartextPassword
	authenticationMD5Password       pgproto3.AuthenticationMD5Password
	authenticationGSS               pgproto3.AuthenticationGSS
	authenticationGSSContinue       pgproto3.AuthenticationGSSContinue
	authenticationSASL              pgproto3.AuthenticationSASL
	authenticationSASLContinue      pgproto3.AuthenticationSASLContinue
	authenticationSASLFinal         pgproto3.AuthenticationSASLFinal
	backendKeyData                  pgproto3.BackendKeyData
	bindComplete                    pgproto3.BindComplete
	closeComplete                   pgproto3.CloseComplete
	commandComplete                 pgproto3.CommandComplete
	copyBothResponse                pgproto3.CopyBothResponse
	copyData                        pgproto3.CopyData
	copyInResponse                  pgproto3.CopyInResponse
	copyOutResponse                 pgproto3.CopyOutResponse
	copyDone                        pgproto3.CopyDone
	dataRow                         pgproto3.DataRow
	emptyQueryResponse              pgproto3.EmptyQueryResponse
	errorResponse                   pgproto3.ErrorResponse
	functionCallResponse            pgproto3.FunctionCallResponse
	noData                          pgproto3.NoData
	noticeResponse                  pgproto3.NoticeResponse
	notificationResponse            pgproto3.NotificationResponse
	parameterDescription            pgproto3.ParameterDescription
	parameterStatus                 pgproto3.ParameterStatus
	parseComplete                   pgproto3.ParseComplete
	readyForQuery                   pgproto3.ReadyForQuery
	rowDescription                  pgproto3.RowDescription
	portalSuspended                 pgproto3.PortalSuspended

}

type StartupPacket struct {
	Length          uint32
	ProtocolVersion uint32
}

type RegularPacket struct {
	Identifier byte
	Length     uint32
	Payload    []byte
}