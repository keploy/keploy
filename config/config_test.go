package config

import (
    "testing"
)


// Test generated using Keploy
func TestLanguageString(t *testing.T) {
    lang := Language("go")
    result := lang.String()
    expected := "go"
    if result != expected {
        t.Errorf("Expected %v, got %v", expected, result)
    }
}

// Test generated using Keploy
func TestLanguageSet_ValidValue(t *testing.T) {
    var lang Language
    err := lang.Set("python")
    if err != nil {
        t.Errorf("Expected no error, got %v", err)
    }
    expected := Language("python")
    if lang != expected {
        t.Errorf("Expected language to be %v, got %v", expected, lang)
    }
}


// Test generated using Keploy
func TestLanguageSet_InvalidValue(t *testing.T) {
    var lang Language
    err := lang.Set("ruby")
    if err == nil {
        t.Errorf("Expected error, got nil")
    }
    expectedError := `must be one of "go", "java", "python" or "javascript"`
    if err.Error() != expectedError {
        t.Errorf("Expected error message %v, got %v", expectedError, err.Error())
    }
}

