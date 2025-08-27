package cli

import (
	"context"
	"testing"

	"errors"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	recordMocks "go.keploy.io/server/v2/pkg/service/record"
	"go.uber.org/zap"
)

// TestRecord_ServiceNotSatisfyingInterface_001 tests the Record function when the service returned by ServiceFactory does not satisfy the recordSvc.Service interface.
func TestRecord_ServiceNotSatisfyingInterface_001(t *testing.T) {
	// Arrange
	ctx := context.Background()
	logger := zap.NewNop()
	mockServiceFactory := new(MockServiceFactory)
	mockCmdConfigurator := new(MockCmdConfigurator)

	mockCmdConfigurator.On("AddFlags", mock.Anything).Return(nil)
	mockCmdConfigurator.On("Validate", mock.Anything, mock.Anything).Return(nil)

	invalidService := struct{}{} // A service that does not satisfy recordSvc.Service
	mockServiceFactory.On("GetService", mock.Anything, "record").Return(invalidService, nil)

	cmd := Record(ctx, logger, nil, mockServiceFactory, mockCmdConfigurator)

	// Act
	err := cmd.ExecuteContext(ctx)

	// Assert
	require.NoError(t, err)
	mockServiceFactory.AssertExpectations(t)
	mockCmdConfigurator.AssertExpectations(t)
}

// TestRecord_ServiceFactoryError_002 tests the Record function when ServiceFactory returns an error while fetching the service.
func TestRecord_ServiceFactoryError_002(t *testing.T) {
	// Arrange
	ctx := context.Background()
	logger := zap.NewNop()
	mockServiceFactory := new(MockServiceFactory)
	mockCmdConfigurator := new(MockCmdConfigurator)

	mockCmdConfigurator.On("AddFlags", mock.Anything).Return(nil)
	mockCmdConfigurator.On("Validate", mock.Anything, mock.Anything).Return(nil)

	mockServiceFactory.On("GetService", mock.Anything, "record").Return(nil, errors.New("service factory error"))

	cmd := Record(ctx, logger, nil, mockServiceFactory, mockCmdConfigurator)

	// Act
	err := cmd.ExecuteContext(ctx)

	// Assert
	require.NoError(t, err)
	mockServiceFactory.AssertExpectations(t)
	mockCmdConfigurator.AssertExpectations(t)
}

// TestRecord_Success_001 ensures that the record command executes successfully
// when all dependencies work as expected.
func TestRecord_Success_001(t *testing.T) {
	// Arrange
	ctx := context.Background()
	logger := zap.NewNop()
	mockServiceFactory := new(MockServiceFactory)
	mockCmdConfigurator := new(MockCmdConfigurator)
	mockRecordService := new(recordMocks.MockService)

	mockCmdConfigurator.On("AddFlags", mock.Anything).Return(nil)
	mockCmdConfigurator.On("Validate", mock.Anything, mock.Anything).Return(nil)

	mockServiceFactory.On("GetService", mock.Anything, "record").Return(mockRecordService, nil)
	mockRecordService.On("Start", mock.Anything, false).Return(nil)

	cmd := Record(ctx, logger, nil, mockServiceFactory, mockCmdConfigurator)
	require.NotNil(t, cmd)

	// Act
	err := cmd.ExecuteContext(ctx)

	// Assert
	require.NoError(t, err)
	mockServiceFactory.AssertExpectations(t)
	mockCmdConfigurator.AssertExpectations(t)
	mockRecordService.AssertExpectations(t)
}

// TestRecord_StartError_002 checks the behavior when the record service's
// Start method returns an error. The command should still exit gracefully.
func TestRecord_StartError_002(t *testing.T) {
	// Arrange
	ctx := context.Background()
	logger := zap.NewNop()
	mockServiceFactory := new(MockServiceFactory)
	mockCmdConfigurator := new(MockCmdConfigurator)
	mockRecordService := new(recordMocks.MockService)
	startErr := errors.New("failed to start record service")

	mockCmdConfigurator.On("AddFlags", mock.Anything).Return(nil)
	mockCmdConfigurator.On("Validate", mock.Anything, mock.Anything).Return(nil)

	mockServiceFactory.On("GetService", mock.Anything, "record").Return(mockRecordService, nil)
	mockRecordService.On("Start", mock.Anything, false).Return(startErr)

	cmd := Record(ctx, logger, nil, mockServiceFactory, mockCmdConfigurator)
	require.NotNil(t, cmd)

	// Act
	err := cmd.ExecuteContext(ctx)

	// Assert
	require.NoError(t, err) // RunE returns nil even on internal errors
	mockServiceFactory.AssertExpectations(t)
	mockCmdConfigurator.AssertExpectations(t)
	mockRecordService.AssertExpectations(t)
}

// TestRecord_AddFlagsError_003 verifies that the Record function returns nil
// if the command configurator fails to add flags.
func TestRecord_AddFlagsError_003(t *testing.T) {
	// Arrange
	ctx := context.Background()
	logger := zap.NewNop()
	mockServiceFactory := new(MockServiceFactory)
	mockCmdConfigurator := new(MockCmdConfigurator)
	flagErr := errors.New("failed to add flags")

	mockCmdConfigurator.On("AddFlags", mock.Anything).Return(flagErr)

	// Act
	cmd := Record(ctx, logger, nil, mockServiceFactory, mockCmdConfigurator)

	// Assert
	require.Nil(t, cmd)
	mockCmdConfigurator.AssertExpectations(t)
}
