package main

import (
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSON2XML(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			orig, err := os.Open(file)
			if err != nil {
				t.Fatal(err)
			}
			got, err := json2xml(orig, "pkg/testing")
			if err != nil {
				t.Fatal(err)
			}

			target := strings.TrimSuffix(file, ".json") + ".xml"
			want, err := ioutil.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, want) {
				t.Errorf("json2xml for %s does not match, got:\n%s\nwant:\n%s\n", name, string(got), string(want))
			}
		})
	}
}
