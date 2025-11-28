# Auto-Generated Mocks for agent Package

## Overview

This directory contains auto-generated mocks for the `agent` package interfaces, created using [mockery](https://github.com/vektra/mockery) v2.53.2.

## Generated Mocks

### `mock_Proxy.go`

Auto-generated mock for the `agent.Proxy` interface.

**Location:** `pkg/agent/mocks/mock_Proxy.go`

**Usage:**
```go
import "go.keploy.io/server/v3/pkg/agent/mocks"

func TestSomething(t *testing.T) {
    mockProxy := mocks.NewMockProxy(t)
    mockProxy.On("Mock", mock.Anything, mock.Anything).Return(nil)
    mockProxy.On("SetMocks", mock.Anything, mock.Anything, mock.Anything).Return(nil)
    
    // Use mockProxy in your tests
    svc := serve.New(logger, cfg, mockProxy)
    
    // Verify calls
    mockProxy.AssertCalled(t, "Mock", mock.Anything, mock.Anything)
}
```

## Regenerating Mocks

If the `agent.Proxy` interface changes, regenerate the mock:

```bash
# Install mockery (if not already installed)
go install github.com/vektra/mockery/v2@v2.53.2

# Regenerate the mock
~/go/bin/mockery \
  --name=Proxy \
  --dir=pkg/agent \
  --output=pkg/agent/mocks \
  --outpkg=mocks \
  --filename=mock_Proxy.go \
  --structname=MockProxy
```

## Important Notes

### Manual Fixes Required

Due to a naming conflict between the embedded `mock.Mock` field and the `Mock()` method from the interface, the generated code requires manual fixes:

1. **Change embedded field to named field:**
   ```go
   // Change from:
   type MockProxy struct {
       mock.Mock
   }
   
   // To:
   type MockProxy struct {
       mockMock mock.Mock
   }
   ```

2. **Update all `_m.Called()` references:**
   ```go
   // Change from:
   ret := _m.Called(ctx, opts)
   
   // To:
   ret := _m.mockMock.Called(ctx, opts)
   ```

3. **Add delegation methods:**
   ```go
   func (_m *MockProxy) On(methodName string, arguments ...interface{}) *mock.Call {
       return _m.mockMock.On(methodName, arguments...)
   }
   
   func (_m *MockProxy) AssertCalled(t mock.TestingT, methodName string, arguments ...interface{}) bool {
       return _m.mockMock.AssertCalled(t, methodName, arguments...)
   }
   ```

4. **Update `NewMockProxy` function:**
   ```go
   m := &MockProxy{}
   m.mockMock.Test(t)
   t.Cleanup(func() { m.mockMock.AssertExpectations(t) })
   ```

### Why Manual Fixes?

The `agent.Proxy` interface has a method named `Mock()`, which conflicts with the embedded `mock.Mock` field. Go doesn't allow a field and method with the same name. Using a named field (`mockMock`) instead of embedding resolves this conflict.

## Future Improvements

Consider:
1. **Using mockery v3** (when stable) which may handle this better
2. **Creating a mockery config file** to automate the generation
3. **Adding a Makefile target** to regenerate mocks easily

## Related Files

- Interface definition: `pkg/agent/service.go`
- Tests using this mock: `pkg/service/serve/serve_test.go`
- Integration tests: `pkg/service/serve/integration_test.go`

