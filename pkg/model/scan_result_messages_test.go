package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeUUID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "32-char hex without dashes is converted to standard format",
			input:    "f285646f4c09aac114505a9aa9dc5e76",
			expected: "f285646f-4c09-aac1-1450-5a9aa9dc5e76",
		},
		{
			name:     "standard 36-char UUID is returned unchanged",
			input:    "f285646f-4c09-aac1-1450-5a9aa9dc5e76",
			expected: "f285646f-4c09-aac1-1450-5a9aa9dc5e76",
		},
		{
			name:     "invalid string is returned unchanged",
			input:    "not-a-uuid",
			expected: "not-a-uuid",
		},
		{
			name:     "empty string is returned unchanged",
			input:    "",
			expected: "",
		},
		{
			name:     "URN format is normalized to standard format",
			input:    "urn:uuid:f285646f-4c09-aac1-1450-5a9aa9dc5e76",
			expected: "f285646f-4c09-aac1-1450-5a9aa9dc5e76",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeUUID(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
