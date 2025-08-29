package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetSelectedTestSets_ValidInput_123 tests the `SetSelectedTestSets` function with valid input to ensure it correctly initializes and populates the `SelectedTestSets` map.
func TestSetSelectedTestSets_ValidInput_123(t *testing.T) {
	// Arrange
	conf := &Config{}
	testSets := []string{"testSet1", "testSet2"}

	// Act
	SetSelectedTestSets(conf, testSets)

	// Assert
	require.NotNil(t, conf.Report.SelectedTestSets)
	assert.Equal(t, 2, len(conf.Report.SelectedTestSets))
	assert.Contains(t, conf.Report.SelectedTestSets, "testSet1")
	assert.Contains(t, conf.Report.SelectedTestSets, "testSet2")
	assert.Empty(t, conf.Report.SelectedTestSets["testSet1"])
	assert.Empty(t, conf.Report.SelectedTestSets["testSet2"])
}

// TestSetSelectedContractTests_ValidInput_456 tests the `SetSelectedContractTests` function with valid input to ensure it correctly sets the `Tests` field.
func TestSetSelectedContractTests_ValidInput_456(t *testing.T) {
	// Arrange
	conf := &Config{}
	tests := []string{"test1", "test2"}

	// Act
	SetSelectedContractTests(conf, tests)

	// Assert
	require.NotNil(t, conf.Contract.Tests)
	assert.Equal(t, 2, len(conf.Contract.Tests))
	assert.Equal(t, "test1", conf.Contract.Tests[0])
	assert.Equal(t, "test2", conf.Contract.Tests[1])
}

// TestGetByPassPorts_ValidInput_789 tests the `GetByPassPorts` function with valid input to ensure it correctly retrieves the list of ports.
func TestGetByPassPorts_ValidInput_789(t *testing.T) {
	// Arrange
	conf := &Config{
		BypassRules: []BypassRule{
			{Port: 8080},
			{Port: 9090},
		},
	}

	// Act
	ports := GetByPassPorts(conf)

	// Assert
	require.NotNil(t, ports)
	assert.Equal(t, 2, len(ports))
	assert.Contains(t, ports, uint(8080))
	assert.Contains(t, ports, uint(9090))
}

// TestSetByPassPorts_ValidInput_321 tests the `SetByPassPorts` function with valid input to ensure it correctly appends ports to the `BypassRules` field.
func TestSetByPassPorts_ValidInput_321(t *testing.T) {
	// Arrange
	conf := &Config{}
	ports := []uint{8080, 9090}

	// Act
	SetByPassPorts(conf, ports)

	// Assert
	require.NotNil(t, conf.BypassRules)
	assert.Equal(t, 2, len(conf.BypassRules))
	assert.Equal(t, uint(8080), conf.BypassRules[0].Port)
	assert.Equal(t, uint(9090), conf.BypassRules[1].Port)
}

// TestLanguage_String_123 tests the String method of the Language type.
func TestLanguage_String_123(t *testing.T) {
	l := Language("go")
	assert.Equal(t, "go", l.String())
}

// TestLanguage_Set_ValidAndInvalid_456 tests setting valid and invalid languages.
func TestLanguage_Set_ValidAndInvalid_456(t *testing.T) {
	var e Language
	// Test valid cases
	valid := []string{"go", "java", "python", "javascript"}
	for _, v := range valid {
		err := e.Set(v)
		require.NoError(t, err)
		assert.Equal(t, Language(v), e)
	}

	// Test invalid case
	err := e.Set("rust")
	require.Error(t, err)
	assert.Equal(t, `must be one of "go", "java", "python" or "javascript"`, err.Error())
}

// TestLanguage_Type_789 tests the Type method of the Language type.
func TestLanguage_Type_789(t *testing.T) {
	var e Language
	assert.Equal(t, "myEnum", e.Type())
}

// TestSetSelectedTests_ValidInput_303 tests the SetSelectedTests function.
func TestSetSelectedTests_ValidInput_303(t *testing.T) {
	conf := &Config{}
	testSets := []string{"test-set-1", "test-set-2"}
	SetSelectedTests(conf, testSets)
	require.NotNil(t, conf.Test.SelectedTests)
	assert.Len(t, conf.Test.SelectedTests, 2)
	assert.Contains(t, conf.Test.SelectedTests, "test-set-1")
	assert.Empty(t, conf.Test.SelectedTests["test-set-1"])
}

// TestSetSelectedServices_ValidInput_404 tests the SetSelectedServices function.
func TestSetSelectedServices_ValidInput_404(t *testing.T) {
	conf := &Config{}
	services := []string{"service-a", "service-b"}
	SetSelectedServices(conf, services)
	assert.Equal(t, services, conf.Contract.Services)
}

// TestSetSelectedTestsNormalize_EmptyInput_864 tests SetSelectedTestsNormalize with an empty string, expecting no error and an empty slice.
func TestSetSelectedTestsNormalize_EmptyInput_864(t *testing.T) {
	// Arrange
	conf := &Config{}
	value := ""

	// Act
	err := SetSelectedTestsNormalize(conf, value)

	// Assert
	require.NoError(t, err)
	assert.Empty(t, conf.Normalize.SelectedTests)
}

// TestSetSelectedTestsNormalize_InvalidFormat_975 tests SetSelectedTestsNormalize with a malformed string, expecting an error.
func TestSetSelectedTestsNormalize_InvalidFormat_975(t *testing.T) {
	// Arrange
	conf := &Config{}
	value := "invalid-format"

	// Act
	err := SetSelectedTestsNormalize(conf, value)

	// Assert
	require.Error(t, err)
	assert.Equal(t, "invalid format: invalid-format", err.Error())
}
