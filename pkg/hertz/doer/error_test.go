package doer

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHertzDoerError_Error(t *testing.T) {
	type fields struct {
		Cause   error
		Kind    string
		Message string
	}
	tests := []struct {
		name   string
		fields fields
		want   string
	}{
		{
			name: "error_with_cause",
			fields: fields{
				Cause:   errors.New("underlying cause"),
				Kind:    "network",
				Message: "failed to connect",
			},
			want: "hertz_doer: network: failed to connect: underlying cause",
		},
		{
			name: "error_without_cause",
			fields: fields{
				Cause:   nil,
				Kind:    "validation",
				Message: "invalid input",
			},
			want: "hertz_doer: validation: invalid input",
		},
		{
			name: "error_with_empty_fields",
			fields: fields{
				Cause:   nil,
				Kind:    "",
				Message: "",
			},
			want: "hertz_doer: : ",
		},
		{
			name: "error_with_only_kind",
			fields: fields{
				Cause:   nil,
				Kind:    "security",
				Message: "",
			},
			want: "hertz_doer: security: ",
		},
		{
			name: "error_with_only_message",
			fields: fields{
				Cause:   nil,
				Kind:    "",
				Message: "an error occurred",
			},
			want: "hertz_doer: : an error occurred",
		},
		{
			name: "error_with_wrapped_error_cause",
			fields: fields{
				Cause:   fmt.Errorf("wrapped: %w", errors.New("original error")),
				Kind:    "database",
				Message: "query failed",
			},
			want: "hertz_doer: database: query failed: wrapped: original error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := &HertzDoerError{
				Cause:   tt.fields.Cause,
				Kind:    tt.fields.Kind,
				Message: tt.fields.Message,
			}
			assert.Equal(t, tt.want, e.Error())
		})
	}
}

func TestHertzDoerError_Unwrap(t *testing.T) {
	baseErr := errors.New("this is the root cause")
	wrappedErr := fmt.Errorf("wrapper: %w", baseErr)

	type fields struct {
		Cause   error
		Kind    string
		Message string
	}
	tests := []struct {
		wantErr error
		fields  fields
		name    string
	}{
		{
			name: "unwrap_with_cause",
			fields: fields{
				Cause:   baseErr,
				Kind:    "test",
				Message: "some message",
			},
			wantErr: baseErr,
		},
		{
			name: "unwrap_without_cause",
			fields: fields{
				Cause:   nil,
				Kind:    "test",
				Message: "another message",
			},
			wantErr: nil,
		},
		{
			name: "unwrap_with_wrapped_cause",
			fields: fields{
				Cause:   wrappedErr,
				Kind:    "test",
				Message: "wrapped error",
			},
			wantErr: wrappedErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := &HertzDoerError{
				Cause:   tt.fields.Cause,
				Kind:    tt.fields.Kind,
				Message: tt.fields.Message,
			}

			// Test Unwrap()
			unwrapped := e.Unwrap()
			assert.Equal(t, tt.wantErr, unwrapped, "Unwrap() should return the correct cause")

			// Test errors.Is()
			if tt.wantErr != nil {
				// errors.Is should find the wrapped error itself
				assert.True(t, errors.Is(e, tt.wantErr), "errors.Is should find the wrapped error")
				// errors.Is should also find the original base error
				assert.True(t, errors.Is(e, baseErr), "errors.Is should trace back to the base error")
			} else {
				assert.False(t, errors.Is(e, baseErr), "errors.Is should not find a base error when there is no cause")
			}
		})
	}
}
