package postmanimport

import "testing"

func TestValidateTestSetName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "users"},
		{name: "users-api"},
		{name: "v1..v2"},
		{name: "foo..bar"},
		{name: "User Auth v1.0"},
		{name: "", wantErr: true},
		{name: "   ", wantErr: true},
		{name: ".", wantErr: true},
		{name: "..", wantErr: true},
		{name: ". ", wantErr: true},
		{name: ".. ", wantErr: true},
		{name: "a/b", wantErr: true},
		{name: `a\b`, wantErr: true},
		{name: "../escaped", wantErr: true},
		{name: `..\escaped`, wantErr: true},
		{name: "foo/../bar", wantErr: true},
		{name: "/tmp/escaped", wantErr: true},
		{name: `C:\escaped`, wantErr: true},
		{name: "C:escaped", wantErr: true},
		{name: "foo\x00bar", wantErr: true},
		{name: "foo\nbar", wantErr: true},
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
