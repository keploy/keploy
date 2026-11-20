package postmanimport

import "testing"

func TestValidateTestSetName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "users"},
		{name: "users-api"},
		{name: ""},
		{name: "..", wantErr: true},
		{name: "../escaped", wantErr: true},
		{name: `..\escaped`, wantErr: true},
		{name: "/tmp/escaped", wantErr: true},
		{name: `C:\\escaped`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTestSetName(tt.name)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateTestSetName(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
		})
	}
}
