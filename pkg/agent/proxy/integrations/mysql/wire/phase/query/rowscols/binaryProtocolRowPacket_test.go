package rowscols

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestEncodeBinaryRow_NullDateTimeFields tests the fix for the issue where
// encoding binary rows with NULL datetime fields caused a panic with error:
// "cannot coerce <nil> to string"
func TestEncodeBinaryRow_NullDateTimeFields(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	tests := []struct {
		name        string
		row         *mysql.BinaryRow
		columns     []*mysql.ColumnDefinition41
		shouldError bool
		description string
	}{
		{
			name: "NULL date_of_birth field (DATE type)",
			row: &mysql.BinaryRow{
				Header: mysql.Header{
					PayloadLength: 0, // Will be computed
					SequenceID:    29,
				},
				Values: []mysql.ColumnEntry{
					{
						Type:     mysql.FieldTypeVarString,
						Name:     "id",
						Value:    "test_id_12345",
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeDate, // Type 10 - DATE
						Name:     "date_of_birth",
						Value:    nil, // NULL value that was causing the error
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeVarString,
						Name:     "status",
						Value:    "active",
						Unsigned: false,
					},
				},
				OkAfterRow:    true,
				RowNullBuffer: []byte{0x08}, // Null bitmap indicating field 1 (date_of_birth) is null - (1+2)%8=3, so bit 3 set: 0x08
			},
			columns: []*mysql.ColumnDefinition41{
				{
					Name: "id",
					Type: byte(mysql.FieldTypeVarString),
				},
				{
					Name: "date_of_birth",
					Type: byte(mysql.FieldTypeDate),
				},
				{
					Name: "status",
					Type: byte(mysql.FieldTypeVarString),
				},
			},
			shouldError: false,
			description: "Should successfully encode a row with NULL DATE field without error",
		},
		{
			name: "NULL datetime field (DATETIME type)",
			row: &mysql.BinaryRow{
				Header: mysql.Header{
					PayloadLength: 0,
					SequenceID:    1,
				},
				Values: []mysql.ColumnEntry{
					{
						Type:     mysql.FieldTypeVarString,
						Name:     "user_id",
						Value:    "user123",
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeDateTime, // Type 7 - DATETIME
						Name:     "last_login",
						Value:    nil, // NULL datetime
						Unsigned: false,
					},
				},
				OkAfterRow:    true,
				RowNullBuffer: []byte{0x08}, // Null bitmap - field 1 (last_login) is null, (1+2)%8=3, so bit 3 set: 0x08
			},
			columns: []*mysql.ColumnDefinition41{
				{
					Name: "user_id",
					Type: byte(mysql.FieldTypeVarString),
				},
				{
					Name: "last_login",
					Type: byte(mysql.FieldTypeDateTime),
				},
			},
			shouldError: false,
			description: "Should successfully encode a row with NULL DATETIME field",
		},
		{
			name: "NULL timestamp field (TIMESTAMP type)",
			row: &mysql.BinaryRow{
				Header: mysql.Header{
					PayloadLength: 0,
					SequenceID:    1,
				},
				Values: []mysql.ColumnEntry{
					{
						Type:     mysql.FieldTypeVarString,
						Name:     "event_id",
						Value:    "evt123",
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeTimestamp, // TIMESTAMP
						Name:     "created_at",
						Value:    nil,
						Unsigned: false,
					},
				},
				OkAfterRow:    true,
				RowNullBuffer: []byte{0x08}, // Null bitmap - field 1 (created_at) is null, (1+2)%8=3, so bit 3 set: 0x08
			},
			columns: []*mysql.ColumnDefinition41{
				{
					Name: "event_id",
					Type: byte(mysql.FieldTypeVarString),
				},
				{
					Name: "created_at",
					Type: byte(mysql.FieldTypeTimestamp),
				},
			},
			shouldError: false,
			description: "Should successfully encode a row with NULL TIMESTAMP field",
		},
		{
			name: "NULL time field (TIME type)",
			row: &mysql.BinaryRow{
				Header: mysql.Header{
					PayloadLength: 0,
					SequenceID:    1,
				},
				Values: []mysql.ColumnEntry{
					{
						Type:     mysql.FieldTypeVarString,
						Name:     "task_id",
						Value:    "task456",
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeTime, // Type 12 - TIME
						Name:     "duration",
						Value:    nil,
						Unsigned: false,
					},
				},
				OkAfterRow:    true,
				RowNullBuffer: []byte{0x08}, // Null bitmap - field 1 (duration) is null, (1+2)%8=3, so bit 3 set: 0x08
			},
			columns: []*mysql.ColumnDefinition41{
				{
					Name: "task_id",
					Type: byte(mysql.FieldTypeVarString),
				},
				{
					Name: "duration",
					Type: byte(mysql.FieldTypeTime),
				},
			},
			shouldError: false,
			description: "Should successfully encode a row with NULL TIME field",
		},
		{
			name: "Multiple NULL datetime fields",
			row: &mysql.BinaryRow{
				Header: mysql.Header{
					PayloadLength: 0,
					SequenceID:    1,
				},
				Values: []mysql.ColumnEntry{
					{
						Type:     mysql.FieldTypeVarString,
						Name:     "id",
						Value:    "record1",
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeDate,
						Name:     "birth_date",
						Value:    nil,
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeDateTime,
						Name:     "created_at",
						Value:    nil,
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeTimestamp,
						Name:     "updated_at",
						Value:    nil,
						Unsigned: false,
					},
				},
				OkAfterRow:    true,
				RowNullBuffer: []byte{0x38}, // Fields 1,2,3 are null: (1+2)%8=3->0x08, (2+2)%8=4->0x10, (3+2)%8=5->0x20 = 0x38
			},
			columns: []*mysql.ColumnDefinition41{
				{Name: "id", Type: byte(mysql.FieldTypeVarString)},
				{Name: "birth_date", Type: byte(mysql.FieldTypeDate)},
				{Name: "created_at", Type: byte(mysql.FieldTypeDateTime)},
				{Name: "updated_at", Type: byte(mysql.FieldTypeTimestamp)},
			},
			shouldError: false,
			description: "Should handle multiple NULL datetime fields in one row",
		},
		{
			name: "Valid datetime with NULL datetime (mixed case)",
			row: &mysql.BinaryRow{
				Header: mysql.Header{
					PayloadLength: 0,
					SequenceID:    1,
				},
				Values: []mysql.ColumnEntry{
					{
						Type:     mysql.FieldTypeDateTime,
						Name:     "created_at",
						Value:    "2024-10-10 11:09:06",
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeDate,
						Name:     "birth_date",
						Value:    nil, // NULL
						Unsigned: false,
					},
					{
						Type:     mysql.FieldTypeDateTime,
						Name:     "updated_at",
						Value:    "2024-10-14 12:20:26",
						Unsigned: false,
					},
				},
				OkAfterRow:    true,
				RowNullBuffer: []byte{0x08}, // Field 1 (birth_date) is null: (1+2)%8=3, so bit 3 set: 0x08
			},
			columns: []*mysql.ColumnDefinition41{
				{Name: "created_at", Type: byte(mysql.FieldTypeDateTime)},
				{Name: "birth_date", Type: byte(mysql.FieldTypeDate)},
				{Name: "updated_at", Type: byte(mysql.FieldTypeDateTime)},
			},
			shouldError: false,
			description: "Should handle mix of valid and NULL datetime fields",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Call the function that was previously failing
			result, err := EncodeBinaryRow(ctx, logger, tt.row, tt.columns)

			if tt.shouldError {
				if err == nil {
					t.Errorf("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v\nDescription: %s", err, tt.description)
				return
			}

			if result == nil {
				t.Error("Expected non-nil result")
				return
			}

			// Verify the result has proper structure (starts with header)
			if len(result) < 4 {
				t.Errorf("Result too short, expected at least 4 bytes for header, got %d", len(result))
			}

			t.Logf("✓ %s - Successfully encoded %d bytes", tt.description, len(result))
		})
	}
}

// TestEncodeBinaryRow_RealWorldExample tests with data structure similar to the actual error log
func TestEncodeBinaryRow_RealWorldExample(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	// This mimics the actual data structure from production that caused the issue
	// The key issue: a row with many columns where one datetime field has a nil value
	// All sensitive data has been replaced with test values
	row := &mysql.BinaryRow{
		Header: mysql.Header{
			PayloadLength: 137,
			SequenceID:    29,
		},
		Values: []mysql.ColumnEntry{
			{Type: mysql.FieldTypeVarString, Name: "id", Value: "test_user_001", Unsigned: false},
			{Type: mysql.FieldTypeDateTime, Name: "created_at", Value: "2024-01-01 10:00:00", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "created_by", Value: "", Unsigned: false},
			{Type: mysql.FieldTypeDateTime, Name: "updated_at", Value: "2024-01-01 10:00:00", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "updated_by", Value: "", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "user_type", Value: "standard", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "contact_number", Value: "5551234567", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "email", Value: "test@example.com", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "first_name", Value: "test_user", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "middle_name", Value: "", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "last_name", Value: "testlast", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "gender", Value: "", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "avatar_url", Value: "", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "tenant_id", Value: "test_tenant_001", Unsigned: false},
			{Type: mysql.FieldTypeLongLong, Name: "deleted_at", Value: 0, Unsigned: true},
			{Type: mysql.FieldTypeDate, Name: "date_of_birth", Value: nil, Unsigned: false}, // THE PROBLEMATIC FIELD - NULL DATE
			{Type: mysql.FieldTypeVarString, Name: "status", Value: "active", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "external_id", Value: "ext_001", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "stage", Value: "onboarding", Unsigned: false},
			{Type: mysql.FieldTypeVarString, Name: "organization", Value: "test_org", Unsigned: false},
		},
		OkAfterRow:    true,
		RowNullBuffer: []byte{0x00, 0x00, 0x02}, // Field 15 (date_of_birth) is null: (15+2)/8=2, (15+2)%8=1, so byte 2, bit 1 = 0x02
	}

	columns := make([]*mysql.ColumnDefinition41, len(row.Values))
	for i, v := range row.Values {
		columns[i] = &mysql.ColumnDefinition41{
			Name: v.Name,
			Type: byte(v.Type),
		}
		if v.Unsigned {
			columns[i].Flags = mysql.UNSIGNED_FLAG
		}
	}

	// This should not error after the fix
	result, err := EncodeBinaryRow(ctx, logger, row, columns)

	if err != nil {
		t.Fatalf("Failed to encode real-world example: %v\nThis is the exact case that caused the original error!", err)
	}

	if result == nil {
		t.Fatal("Expected non-nil result for real-world example")
	}

	t.Logf("✓ Successfully encoded real-world example with NULL date_of_birth field: %d bytes", len(result))
}

// TestCoerceToString tests the coerceToString helper function
func TestCoerceToString(t *testing.T) {
	tests := []struct {
		name        string
		input       interface{}
		shouldError bool
		expected    string
	}{
		{
			name:        "nil value should error",
			input:       nil,
			shouldError: true,
		},
		{
			name:        "string value",
			input:       "test",
			shouldError: false,
			expected:    "test",
		},
		{
			name:        "byte slice",
			input:       []byte("bytes"),
			shouldError: false,
			expected:    "bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := coerceToString(tt.input)

			if tt.shouldError {
				if err == nil {
					t.Errorf("Expected error for %v, but got none", tt.input)
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			if result != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, result)
			}
		})
	}
}
