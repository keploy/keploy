package models

import (
	"github.com/jackc/pgproto3/v2"
)

const ProtocolVersionNumber uint32 = 196608 // Replace with actual version number if different

// PG Request Packet Transcoder
type Backend struct {
	PacketTypes         []string                     `json:"header,omitempty" yaml:"header,omitempty"`
	Identfier           string                       `json:"identifier,omitempty" yaml:"identifier,omitempty"`
	Length              uint32                       `json:"length,omitempty" yaml:"length,omitempty"`
	Payload             string                       `json:"payload,omitempty" yaml:"payload,omitempty"`
	Bind                pgproto3.Bind                `json:"bind,omitempty" yaml:"bind,omitempty"`
	CancelRequest       pgproto3.CancelRequest       `json:"cancel_request,omitempty" yaml:"cancel_request,omitempty"`
	Close               pgproto3.Close               `json:"close,omitempty" yaml:"close,omitempty"`
	CopyFail            pgproto3.CopyFail            `json:"copy_fail,omitempty" yaml:"copy_fail,omitempty"`
	CopyData            pgproto3.CopyData            `json:"copy_data,omitempty" yaml:"copy_data,omitempty"`
	CopyDone            pgproto3.CopyDone            `json:"copy_done,omitempty" yaml:"copy_done,omitempty"`
	Describe            pgproto3.Describe            `json:"describe,omitempty" yaml:"describe,omitempty"`
	Execute             pgproto3.Execute             `json:"execute,omitempty" yaml:"execute,omitempty"`
	Flush               pgproto3.Flush               `json:"flush,omitempty" yaml:"flush,omitempty"`
	FunctionCall        pgproto3.FunctionCall        `json:"function_call,omitempty" yaml:"function_call,omitempty"`
	GssEncRequest       pgproto3.GSSEncRequest       `json:"gss_enc_request,omitempty" yaml:"gss_enc_request,omitempty"`
	Parse               pgproto3.Parse               `json:"parse,omitempty" yaml:"parse,omitempty"`
	Query               pgproto3.Query               `json:"query,omitempty" yaml:"query,omitempty"`
	SSlRequest          pgproto3.SSLRequest          `json:"ssl_request,omitempty" yaml:"ssl_request,omitempty"`
	StartupMessage      pgproto3.StartupMessage      `json:"startup_message,omitempty" yaml:"startup_message,omitempty"`
	Sync                pgproto3.Sync                `json:"sync,omitempty" yaml:"sync,omitempty"`
	Terminate           pgproto3.Terminate           `json:"terminate,omitempty" yaml:"terminate,omitempty"`
	SASLInitialResponse pgproto3.SASLInitialResponse `json:"sasl_initial_response,omitempty" yaml:"sasl_initial_response,omitempty"`
	SASLResponse        pgproto3.SASLResponse        `json:"sasl_response,omitempty" yaml:"sasl_response,omitempty"`
	PasswordMessage     pgproto3.PasswordMessage     `json:"password_message,omitempty" yaml:"password_message,omitempty"`
	MsgType             byte                         `json:"msg_type,omitempty" yaml:"msg_type,omitempty"`
	PartialMsg          bool                         `json:"partial_msg,omitempty" yaml:"partial_msg,omitempty"`
	AuthType            int32                        `json:"auth_type" yaml:"auth_type"`
	BodyLen             int                          `json:"body_len,omitempty" yaml:"body_len,omitempty"`
	// AuthMechanism       string                       `json:"auth_mechanism,omitempty" yaml:"auth_mechanism,omitempty"`
}

// func NewBackend() *Backend {
// 	return &Backend{}
// }

type Frontend struct {
	PacketTypes                     []string                                 `json:"header,omitempty" yaml:"header,omitempty"`
	Identfier                       string                                   `json:"identifier,omitempty" yaml:"identifier,omitempty"`
	Length                          uint32                                   `json:"length,omitempty" yaml:"length,omitempty"`
	Payload                         string                                   `json:"payload,omitempty" yaml:"payload,omitempty"`
	AuthenticationOk                pgproto3.AuthenticationOk                `json:"authentication_ok,omitempty" yaml:"authentication_ok,omitempty"`
	AuthenticationCleartextPassword pgproto3.AuthenticationCleartextPassword `json:"authentication_cleartext_password,omitempty" yaml:"authentication_cleartext_password,omitempty"`
	AuthenticationMD5Password       pgproto3.AuthenticationMD5Password       `json:"authentication_md5_password,omitempty" yaml:"authentication_md5_password,omitempty"`
	AuthenticationGSS               pgproto3.AuthenticationGSS               `json:"authentication_gss,omitempty" yaml:"authentication_gss,omitempty"`
	AuthenticationGSSContinue       pgproto3.AuthenticationGSSContinue       `json:"authentication_gss_continue,omitempty" yaml:"authentication_gss_continue,omitempty"`
	AuthenticationSASL              pgproto3.AuthenticationSASL              `json:"authentication_sasl,omitempty" yaml:"authentication_sasl,omitempty"`
	AuthenticationSASLContinue      pgproto3.AuthenticationSASLContinue      `json:"authentication_sasl_continue,omitempty" yaml:"authentication_sasl_continue,omitempty"`
	AuthenticationSASLFinal         pgproto3.AuthenticationSASLFinal         `json:"authentication_sasl_final,omitempty" yaml:"authentication_sasl_final,omitempty"`
	BackendKeyData                  pgproto3.BackendKeyData                  `json:"backend_key_data,omitempty" yaml:"backend_key_data,omitempty"`
	BindComplete                    pgproto3.BindComplete                    `json:"bind_complete,omitempty" yaml:"bind_complete,omitempty"`
	CloseComplete                   pgproto3.CloseComplete                   `json:"close_complete,omitempty" yaml:"close_complete,omitempty"`
	CommandComplete                 pgproto3.CommandComplete                 `json:"command_complete,omitempty" yaml:"command_complete,omitempty"`
	CopyBothResponse                pgproto3.CopyBothResponse                `json:"copy_both_response,omitempty" yaml:"copy_both_response,omitempty"`
	CopyData                        pgproto3.CopyData                        `json:"copy_data,omitempty" yaml:"copy_data,omitempty"`
	CopyInResponse                  pgproto3.CopyInResponse                  `json:"copy_in_response,omitempty" yaml:"copy_in_response,omitempty"`
	CopyOutResponse                 pgproto3.CopyOutResponse                 `json:"copy_out_response,omitempty" yaml:"copy_out_response,omitempty"`
	CopyDone                        pgproto3.CopyDone                        `json:"copy_done,omitempty" yaml:"copy_done,omitempty"`
	DataRow                         pgproto3.DataRow                         `json:"data_row,omitempty" yaml:"data_row,omitempty"`
	EmptyQueryResponse              pgproto3.EmptyQueryResponse              `json:"empty_query_response,omitempty" yaml:"empty_query_response,omitempty"`
	ErrorResponse                   pgproto3.ErrorResponse                   `json:"error_response,omitempty" yaml:"error_response,omitempty"`
	FunctionCallResponse            pgproto3.FunctionCallResponse            `json:"function_call_response,omitempty" yaml:"function_call_response,omitempty"`
	NoData                          pgproto3.NoData                          `json:"no_data,omitempty" yaml:"no_data,omitempty"`
	NoticeResponse                  pgproto3.NoticeResponse                  `json:"notice_response,omitempty" yaml:"notice_response,omitempty"`
	NotificationResponse            pgproto3.NotificationResponse            `json:"notification_response,omitempty" yaml:"notification_response,omitempty"`
	ParameterDescription            pgproto3.ParameterDescription            `json:"parameter_description,omitempty" yaml:"parameter_description,omitempty"`
	ParameterStatus                 pgproto3.ParameterStatus                 `yaml:"-"`
	ParameterStatusCombined         []pgproto3.ParameterStatus               `json:"parameter_status,omitempty" yaml:"parameter_status,omitempty"`
	ParseComplete                   pgproto3.ParseComplete                   `json:"parse_complete,omitempty" yaml:"parse_complete,omitempty"`
	ReadyForQuery                   pgproto3.ReadyForQuery                   `json:"ready_for_query,omitempty" yaml:"ready_for_query,omitempty"`
	RowDescription                  pgproto3.RowDescription                  `json:"row_description,omitempty" yaml:"row_description,omitempty"`
	PortalSuspended                 pgproto3.PortalSuspended                 `json:"portal_suspended,omitempty" yaml:"portal_suspended,omitempty"`
	MsgType                         byte                                     `json:"msg_type,omitempty" yaml:"msg_type,omitempty"`
	AuthType                        int32                                    `json:"auth_type" yaml:"auth_type"`
	// AuthMechanism                   string                                   `json:"auth_mechanism,omitempty" yaml:"auth_mechanism,omitempty"`
	BodyLen int `json:"body_len,omitempty" yaml:"body_len,omitempty"`
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

const (
	minStartupPacketLen = 4     // minStartupPacketLen is a single 32-bit int version or code.
	maxStartupPacketLen = 10000 // maxStartupPacketLen is MAX_STARTUP_PACKET_LENGTH from PG source.
	sslRequestNumber    = 80877103
	cancelRequestCode   = 80877102
	gssEncReqNumber     = 80877104
)
