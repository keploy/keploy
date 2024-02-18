package models

import "fmt"

type IncomingError struct {
	Err error
}

func (e IncomingError) Error() string {
	return fmt.Sprintf("incoming error: %v", e.Err)
}

type OutgoingError struct {
	Err error
}

func (e OutgoingError) Error() string {
	return fmt.Sprintf("outgoing error: %v", e.Err)
}

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

const (
	ErrInterrupted    AppErrorType = "exited with interrupt"
	ErrCommandError   AppErrorType = "exited due to command error"
	ErrUnExpected     AppErrorType = "an unexpected error occurred"
	ErrDockerError    AppErrorType = "an error occurred while using docker client"
	ErrFailedUnitTest AppErrorType = "test failure occurred when running keploy tests along with unit tests"
	ErrKilledByKeploy AppErrorType = "killed by keploy"
)
