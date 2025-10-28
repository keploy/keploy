package models

import "fmt"

type AppError struct {
	AppErrorType AppErrorType
	Err          error
}

type AppErrorType string

func (e AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.AppErrorType, e.Err)
	}
	return string(e.AppErrorType)
}

// AppErrorType is a type of error that can be returned by the application
const (
	ErrCommandError   AppErrorType = "exited due to command error"
	ErrUnExpected     AppErrorType = "an unexpected error occurred"
	ErrInternal       AppErrorType = "an internal error occurred"
	ErrAppStopped     AppErrorType = "app stopped"
	ErrCtxCanceled    AppErrorType = "context canceled"
	ErrTestBinStopped AppErrorType = "test binary stopped"
)

type ParserError struct {
	ParserErrorType ParserErrorType
	Err             error
}

type ParserErrorType string

func (e ParserError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.ParserErrorType, e.Err)
	}
	return string(e.ParserErrorType)
}

const (
	ErrMockNotFound ParserErrorType = "mock not found"
)
