package main

import (
	"fmt"
	"os"
	"testing"
)

func TestShouldWrap(t *testing.T) {
	var tests = []struct {
		envs       map[string]string
		shouldWrap bool
	}{
		{
			envs: map[string]string{
				"GO_TEST_WRAP":    "0",
				"XML_OUTPUT_FILE": "",
			},
			shouldWrap: false,
		}, {
			envs: map[string]string{
				"GO_TEST_WRAP":    "1",
				"XML_OUTPUT_FILE": "",
			},
			shouldWrap: true,
		}, {
			envs: map[string]string{
				"GO_TEST_WRAP":    "",
				"XML_OUTPUT_FILE": "path",
			},
			shouldWrap: true,
		},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%v", tt.envs), func(t *testing.T) {
			for k, v := range tt.envs {
				if v == "" {
					os.Unsetenv(k)
				} else {
					os.Setenv(k, v)
				}
			}
			got := shouldWrap()
			if tt.shouldWrap != got {
				t.Errorf("shouldWrap returned %t, expected %t", got, tt.shouldWrap)
			}
		})
	}
}
